package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/webhook"
)

// fakeAdmin is the in-memory TokenAdmin used by run_test.go. It models
// the real Postgres semantics that matter for the run path:
//
//   - At most one row with revoked_at == nil per (channel, token_hash).
//     Insert against an active row returns ErrTokenAlreadyActive (this
//     is what the partial unique index on webhook_tokens enforces).
//   - ScheduleRevocation flips the active row to a future revoked_at.
//   - A token is "valid at t" iff its revoked_at is nil OR t.Before(*revoked_at).
//
// This fake is the documented in-memory adapter the test rules permit
// — it mirrors production semantics for the Insert / ScheduleRevocation
// surface; the runtime Lookup path is covered by store_test.go against
// the real SQL.
type fakeAdmin struct {
	mu          sync.Mutex
	rows        []fakeRow
	insertErr   error  // forced error on next Insert (cleared after use)
	scheduleErr error  // forced error on next ScheduleRevocation
	insertHook  func() // invoked at the start of Insert (lets tests assert ordering)
}

type fakeRow struct {
	tenantID       webhook.TenantID
	channel        string
	tokenHash      []byte
	overlapMinutes int
	createdAt      time.Time
	revokedAt      *time.Time
}

func (f *fakeAdmin) Insert(_ context.Context, tenantID webhook.TenantID, channel string, tokenHash []byte, overlapMinutes int, createdAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertHook != nil {
		f.insertHook()
	}
	if err := f.insertErr; err != nil {
		f.insertErr = nil
		return err
	}
	for _, r := range f.rows {
		if r.channel != channel || !bytes.Equal(r.tokenHash, tokenHash) {
			continue
		}
		if r.revokedAt == nil {
			return webhook.ErrTokenAlreadyActive
		}
	}
	f.rows = append(f.rows, fakeRow{
		tenantID:       tenantID,
		channel:        channel,
		tokenHash:      append([]byte(nil), tokenHash...),
		overlapMinutes: overlapMinutes,
		createdAt:      createdAt,
	})
	return nil
}

func (f *fakeAdmin) ScheduleRevocation(_ context.Context, channel string, tokenHash []byte, effectiveAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.scheduleErr; err != nil {
		f.scheduleErr = nil
		return err
	}
	for i := range f.rows {
		r := &f.rows[i]
		if r.channel != channel || !bytes.Equal(r.tokenHash, tokenHash) || r.revokedAt != nil {
			continue
		}
		when := effectiveAt
		r.revokedAt = &when
		return nil
	}
	return webhook.ErrTokenNotFound
}

// validAt returns the unique row authoritative at instant t for
// (channel, tokenHash). Used by the rotation tests to assert "old
// token still valid mid-grace, dead after grace".
func (f *fakeAdmin) validAt(channel string, tokenHash []byte, t time.Time) (fakeRow, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.channel != channel || !bytes.Equal(r.tokenHash, tokenHash) {
			continue
		}
		if r.revokedAt == nil || t.Before(*r.revokedAt) {
			return r, true
		}
	}
	return fakeRow{}, false
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

const (
	tenantUUID    = "11111111-2222-3333-4444-555555555555"
	rotatedTenant = "11111111-2222-3333-4444-555555555555"
)

// TestRun_MintHappyPath asserts the canonical flow: a valid mint
// inserts exactly one active row, prints plaintext + hash + URL, and
// returns nil.
func TestRun_MintHappyPath(t *testing.T) {
	t.Parallel()
	admin := &fakeAdmin{}
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	var stdout, stderr bytes.Buffer

	err := Run(context.Background(), admin, fixedClock{now}, Options{
		Channel:        "whatsapp",
		TenantID:       tenantUUID,
		OverlapMinutes: 5,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(admin.rows); got != 1 {
		t.Fatalf("rows = %d, want 1", got)
	}
	if got := admin.rows[0].channel; got != "whatsapp" {
		t.Fatalf("channel = %q", got)
	}
	if !admin.rows[0].createdAt.Equal(now) {
		t.Fatalf("createdAt = %v, want %v", admin.rows[0].createdAt, now)
	}

	// Plaintext is on stdout, exactly once.
	out := stdout.String()
	if !strings.Contains(out, "TOKEN PLAINTEXT") {
		t.Fatalf("stdout missing token banner:\n%s", out)
	}
	// Pull the printed plaintext (the line after the banner).
	lines := strings.Split(out, "\n")
	var plaintext string
	for i, l := range lines {
		if strings.Contains(l, "TOKEN PLAINTEXT") && i+1 < len(lines) {
			plaintext = strings.TrimSpace(lines[i+1])
			break
		}
	}
	if len(plaintext) != 64 {
		t.Fatalf("plaintext on stdout has unexpected length %d: %q", len(plaintext), plaintext)
	}
	want := sha256.Sum256([]byte(plaintext))
	if !bytes.Equal(admin.rows[0].tokenHash, want[:]) {
		t.Fatalf("stored hash != sha256(plaintext) — mint/lookup contract broken")
	}
	// URL must include the plaintext, never the hash.
	if !strings.Contains(out, "/webhooks/whatsapp/"+plaintext) {
		t.Fatalf("missing URL line:\n%s", out)
	}
	if strings.Contains(out, hex.EncodeToString(want[:])) && !strings.Contains(out, "hash (hex):") {
		t.Fatalf("hash hex appeared without label, scan output for accidents")
	}
}

// TestRun_MintAlreadyActive asserts the operator gets a typed error
// when the partial-unique-index trips. (We force this by injecting an
// existing active row with the hash that the next mint would produce —
// since we cannot predict the random plaintext, we instead pre-seed a
// row and assert behaviour against a SECOND mint of a known plaintext
// via the admin Insert path directly.)
func TestRun_MintAlreadyActive(t *testing.T) {
	t.Parallel()
	admin := &fakeAdmin{}
	admin.insertErr = webhook.ErrTokenAlreadyActive

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), admin, fixedClock{time.Now()}, Options{
		Channel:  "whatsapp",
		TenantID: tenantUUID,
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error on already-active hash")
	}
	if !errors.Is(err, webhook.ErrTokenAlreadyActive) {
		t.Fatalf("err = %v, want ErrTokenAlreadyActive", err)
	}
	if strings.Contains(stdout.String(), "TOKEN PLAINTEXT") {
		t.Fatal("plaintext leaked even though insert failed")
	}
}

// TestRun_MintGenericInsertFailure verifies that non-typed Insert
// errors (e.g. driver loss) propagate without the "already active"
// hint, so operators don't get misleading guidance.
func TestRun_MintGenericInsertFailure(t *testing.T) {
	t.Parallel()
	admin := &fakeAdmin{}
	admin.insertErr = errors.New("connection lost")

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), admin, fixedClock{time.Now()}, Options{
		Channel:  "whatsapp",
		TenantID: tenantUUID,
	}, &stdout, &stderr)
	if err == nil || strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err should be generic insert error, got %v", err)
	}
	if errors.Is(err, webhook.ErrTokenAlreadyActive) {
		t.Fatalf("err must NOT match ErrTokenAlreadyActive")
	}
}

// TestRun_RotateGraceWindow seeds an active token, then calls Run with
// --rotate-from-token-hash-hex pointing at it. Within the overlap
// window both the old and new hashes are valid; after the window the
// old one is revoked and only the new one resolves.
func TestRun_RotateGraceWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	admin := &fakeAdmin{}
	tenant, err := webhook.ParseTenantID(tenantUUID)
	if err != nil {
		t.Fatal(err)
	}
	// Pre-seed an active row.
	oldHash := sha256.Sum256([]byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"))
	if err := admin.Insert(context.Background(), tenant, "whatsapp", oldHash[:], 0, now.Add(-time.Hour)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err = Run(context.Background(), admin, fixedClock{now}, Options{
		Channel:                "whatsapp",
		TenantID:               rotatedTenant,
		OverlapMinutes:         5,
		RotateFromTokenHashHex: hex.EncodeToString(oldHash[:]),
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(admin.rows); got != 2 {
		t.Fatalf("rows = %d, want 2 (old scheduled-revoked + new active)", got)
	}
	if !strings.Contains(stdout.String(), "rotation:    OLD") {
		t.Fatalf("missing rotation summary in stdout:\n%s", stdout.String())
	}

	// Within the grace window both hashes resolve.
	if _, ok := admin.validAt("whatsapp", oldHash[:], now.Add(2*time.Minute)); !ok {
		t.Fatal("old hash should still be valid 2 minutes into the 5-minute grace window")
	}
	// After the grace window only the new hash resolves.
	afterGrace := now.Add(6 * time.Minute)
	if _, ok := admin.validAt("whatsapp", oldHash[:], afterGrace); ok {
		t.Fatal("old hash should be revoked after the grace window")
	}
	// New hash is the most recent inserted row.
	newRow := admin.rows[1]
	if _, ok := admin.validAt("whatsapp", newRow.tokenHash, afterGrace); !ok {
		t.Fatal("new hash should remain valid after the grace window")
	}
}

// TestRun_RotateImmediateCut covers overlap_minutes=0: the old token
// becomes invalid the instant the new one is minted.
func TestRun_RotateImmediateCut(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	admin := &fakeAdmin{}
	tenant, _ := webhook.ParseTenantID(tenantUUID)
	oldHash := sha256.Sum256([]byte("0000000000000000000000000000000000000000000000000000000000000000"))
	_ = admin.Insert(context.Background(), tenant, "whatsapp", oldHash[:], 0, now.Add(-time.Hour))

	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), admin, fixedClock{now}, Options{
		Channel:                "whatsapp",
		TenantID:               tenantUUID,
		OverlapMinutes:         0,
		RotateFromTokenHashHex: hex.EncodeToString(oldHash[:]),
	}, &stdout, &stderr); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// At instant `now` the old token is exactly at revoked_at; per
	// the runtime Lookup contract that's already revoked.
	if _, ok := admin.validAt("whatsapp", oldHash[:], now); ok {
		t.Fatal("overlap=0 should cut the old token at `now` exactly")
	}
}

// TestRun_RotateFailureLeavesNewRowAndReports verifies the "new row
// exists, old revocation failed" failure mode: stderr must tell the
// operator how to clean up, and the returned error must mention the
// schedule-revocation failure.
func TestRun_RotateFailureLeavesNewRowAndReports(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	admin := &fakeAdmin{}
	admin.scheduleErr = errors.New("connection lost mid-rotation")
	oldHash := sha256.Sum256([]byte("dead"))

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), admin, fixedClock{now}, Options{
		Channel:                "whatsapp",
		TenantID:               tenantUUID,
		OverlapMinutes:         5,
		RotateFromTokenHashHex: hex.EncodeToString(oldHash[:]),
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when ScheduleRevocation fails")
	}
	if !strings.Contains(stderr.String(), "WARNING") || !strings.Contains(stderr.String(), "DELETE FROM webhook_tokens") {
		t.Fatalf("stderr missing operator-cleanup hint:\n%s", stderr.String())
	}
	if got := len(admin.rows); got != 1 {
		t.Fatalf("new row should exist (admin has %d rows)", got)
	}
}

// TestRun_RotateUnknownOldHash exercises the operator-typo path: the
// old hash does not match any active row. ScheduleRevocation returns
// ErrTokenNotFound; Run surfaces it.
func TestRun_RotateUnknownOldHash(t *testing.T) {
	t.Parallel()
	admin := &fakeAdmin{}
	oldHash := sha256.Sum256([]byte("never-seen"))
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), admin, fixedClock{time.Now()}, Options{
		Channel:                "whatsapp",
		TenantID:               tenantUUID,
		OverlapMinutes:         5,
		RotateFromTokenHashHex: hex.EncodeToString(oldHash[:]),
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for unknown old hash")
	}
	if !errors.Is(err, webhook.ErrTokenNotFound) {
		t.Fatalf("err = %v, want ErrTokenNotFound", err)
	}
}

// TestOptionsValidate covers the rejection branches the run path
// short-circuits before any storage call.
func TestOptionsValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		o    Options
		want string
	}{
		{"empty channel", Options{Channel: "", TenantID: tenantUUID}, "--channel"},
		{"bad channel charset", Options{Channel: "What:App", TenantID: tenantUUID}, "--channel"},
		{"bad tenant", Options{Channel: "whatsapp", TenantID: "not-a-uuid"}, "--tenant-id"},
		{"negative overlap", Options{Channel: "whatsapp", TenantID: tenantUUID, OverlapMinutes: -1}, "--overlap-minutes"},
		{"bad rotate hex (length)", Options{Channel: "whatsapp", TenantID: tenantUUID, RotateFromTokenHashHex: "abcd"}, "--rotate-from-token-hash-hex"},
		{"bad rotate hex (chars)", Options{Channel: "whatsapp", TenantID: tenantUUID, RotateFromTokenHashHex: strings.Repeat("zz", 32)}, "--rotate-from-token-hash-hex"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.o.validate()
			if err == nil {
				t.Fatalf("validate(%+v) returned nil, want error containing %q", tc.o, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// TestOptionsValidate_AcceptsCanonicalInputs covers the happy path
// for both mint-only and rotate options. A whitespace-padded hash hex
// is accepted because operators paste from a previous mint output.
func TestOptionsValidate_AcceptsCanonicalInputs(t *testing.T) {
	t.Parallel()
	hashHex := strings.Repeat("ab", 32)
	cases := []Options{
		{Channel: "whatsapp", TenantID: tenantUUID, OverlapMinutes: 0},
		{Channel: "instagram_msgr", TenantID: tenantUUID, OverlapMinutes: 5},
		{Channel: "whatsapp", TenantID: tenantUUID, OverlapMinutes: 5, RotateFromTokenHashHex: " " + hashHex + "\n"},
	}
	for i, c := range cases {
		c := c
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			t.Parallel()
			if err := c.validate(); err != nil {
				t.Fatalf("validate(%+v): %v", c, err)
			}
		})
	}
}

// TestDecodeHashHex_Errors covers the input shapes operators most
// often paste wrong.
func TestDecodeHashHex_Errors(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"abcd",
		strings.Repeat("zz", 32),
	}
	for _, c := range cases {
		c := c
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			if _, err := decodeHashHex(c); err == nil {
				t.Fatalf("decodeHashHex(%q): want error", c)
			}
		})
	}
}
