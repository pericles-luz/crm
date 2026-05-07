//go:build integration

package integration_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	"github.com/pericles-luz/crm/internal/customdomain/validation"
	"github.com/pericles-luz/crm/internal/iam/dnsresolver"
)

// fakeResolver is the package-local deterministic dnsresolver.Resolver
// used by these integration tests. It mirrors the unit-test resolver in
// internal/customdomain/validation/validate_test.go but lives here so
// the integration build tag does not pull validation-package internals.
type fakeResolver struct {
	ipAnswers map[string][]dnsresolver.IPAnswer
	ipErrs    map[string]error
	txtAns    map[string][]string
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{
		ipAnswers: map[string][]dnsresolver.IPAnswer{},
		ipErrs:    map[string]error{},
		txtAns:    map[string][]string{},
	}
}

func (f *fakeResolver) LookupIP(_ context.Context, host string) ([]dnsresolver.IPAnswer, error) {
	if e, ok := f.ipErrs[host]; ok {
		return nil, e
	}
	return f.ipAnswers[host], nil
}

func (f *fakeResolver) LookupTXT(_ context.Context, host string) ([]string, error) {
	return f.txtAns[host], nil
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// noopAuditor satisfies validation.Auditor for tests that focus on
// dns_resolution_log writes. The real Auditor adapter is exercised
// elsewhere.
type noopAuditor struct{}

func (noopAuditor) Record(context.Context, validation.AuditEvent) {}

type dnsLogRow struct {
	tenantID           *uuid.UUID
	host               string
	pinnedIP           *string
	verifiedWithDNSSEC bool
	decision           string
	reason             string
	phase              string
	createdAt          time.Time
}

func fetchDNSLogRows(t *testing.T, h *harness, host string) []dnsLogRow {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := h.pool.Query(ctx, `
SELECT tenant_id, host, host(pinned_ip), verified_with_dnssec,
       decision, reason, phase, created_at
  FROM dns_resolution_log
 WHERE host = $1
 ORDER BY created_at ASC, id ASC
`, host)
	if err != nil {
		t.Fatalf("query dns_resolution_log: %v", err)
	}
	defer rows.Close()
	var out []dnsLogRow
	for rows.Next() {
		var (
			tenantID *[16]byte
			row      dnsLogRow
		)
		if err := rows.Scan(&tenantID, &row.host, &row.pinnedIP, &row.verifiedWithDNSSEC, &row.decision, &row.reason, &row.phase, &row.createdAt); err != nil {
			t.Fatalf("scan dns_resolution_log: %v", err)
		}
		if tenantID != nil {
			id := uuid.UUID(*tenantID)
			row.tenantID = &id
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	return out
}

func TestDNSResolutionLog_BlockedSSRF_PersistsRowWithoutIP(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)

	store := pgstore.NewDNSResolutionLogStore(h.pool)
	resolver := newFakeResolver()
	resolver.ipAnswers["evil.example"] = []dnsresolver.IPAnswer{{IP: mustAddr(t, "127.0.0.1")}}
	clock := fixedClock{t: time.Date(2026, 5, 7, 9, 0, 0, 0, time.UTC)}
	v := validation.New(resolver, noopAuditor{}, clock, validation.WithWriter(store))

	tenantID := uuid.New()
	ctx, cancel := context.WithTimeout(validation.WithTenantID(context.Background(), tenantID), 5*time.Second)
	defer cancel()

	_, err := v.Validate(ctx, "evil.example", "tok")
	if !errors.Is(err, validation.ErrPrivateIP) {
		t.Fatalf("err = %v, want ErrPrivateIP", err)
	}

	rows := fetchDNSLogRows(t, h, "evil.example")
	if len(rows) != 1 {
		t.Fatalf("expected 1 dns_resolution_log row, got %d", len(rows))
	}
	row := rows[0]
	if row.tenantID == nil || *row.tenantID != tenantID {
		t.Fatalf("tenant_id = %v, want %s", row.tenantID, tenantID)
	}
	if row.pinnedIP != nil {
		t.Fatalf("pinned_ip leaked attacker-chosen address: %s", *row.pinnedIP)
	}
	if row.decision != "block" {
		t.Fatalf("decision = %s, want block", row.decision)
	}
	if row.reason != "private_ip" {
		t.Fatalf("reason = %s, want private_ip", row.reason)
	}
	if row.phase != "validate" {
		t.Fatalf("phase = %s, want validate", row.phase)
	}
	if !row.createdAt.Equal(clock.t) {
		t.Fatalf("created_at = %v, want %v", row.createdAt, clock.t)
	}
}

func TestDNSResolutionLog_HappyPath_PersistsAllowRowWithIP(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)

	store := pgstore.NewDNSResolutionLogStore(h.pool)
	resolver := newFakeResolver()
	resolver.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{
		{IP: mustAddr(t, "203.0.113.10"), VerifiedWithDNSSEC: true},
	}
	resolver.txtAns["_crm-verify.acme.example"] = []string{"tok"}
	clock := fixedClock{t: time.Date(2026, 5, 7, 9, 30, 0, 0, time.UTC)}
	v := validation.New(resolver, noopAuditor{}, clock, validation.WithWriter(store))

	tenantID := uuid.New()
	ctx, cancel := context.WithTimeout(validation.WithTenantID(context.Background(), tenantID), 5*time.Second)
	defer cancel()

	if _, err := v.Validate(ctx, "acme.example", "tok"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rows := fetchDNSLogRows(t, h, "acme.example")
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]
	if row.decision != "allow" || row.reason != "ok" {
		t.Fatalf("entry = %+v", row)
	}
	if row.pinnedIP == nil || *row.pinnedIP != "203.0.113.10" {
		t.Fatalf("pinned_ip = %v, want 203.0.113.10", row.pinnedIP)
	}
	if !row.verifiedWithDNSSEC {
		t.Fatalf("verified_with_dnssec should be true")
	}
}

func TestDNSResolutionLog_HostOnly_AllowEmitsRowWithoutIP(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)

	store := pgstore.NewDNSResolutionLogStore(h.pool)
	resolver := newFakeResolver()
	resolver.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{{IP: mustAddr(t, "203.0.113.10")}}
	clock := fixedClock{t: time.Date(2026, 5, 7, 9, 45, 0, 0, time.UTC)}
	v := validation.New(resolver, noopAuditor{}, clock, validation.WithWriter(store))

	if err := v.ValidateHostOnly(context.Background(), "acme.example"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rows := fetchDNSLogRows(t, h, "acme.example")
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]
	if row.phase != "host_only" {
		t.Fatalf("phase = %s, want host_only", row.phase)
	}
	if row.decision != "allow" || row.reason != "ok" {
		t.Fatalf("entry = %+v", row)
	}
	if row.pinnedIP != nil {
		t.Fatalf("ValidateHostOnly must not pin an IP, got %s", *row.pinnedIP)
	}
}

func TestDNSResolutionLog_AnonymousCallStoresNullTenantID(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)

	store := pgstore.NewDNSResolutionLogStore(h.pool)
	resolver := newFakeResolver()
	resolver.ipAnswers["anon.example"] = []dnsresolver.IPAnswer{{IP: mustAddr(t, "203.0.113.10")}}
	resolver.txtAns["_crm-verify.anon.example"] = []string{"tok"}
	clock := fixedClock{t: time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)}
	v := validation.New(resolver, noopAuditor{}, clock, validation.WithWriter(store))

	if _, err := v.Validate(context.Background(), "anon.example", "tok"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rows := fetchDNSLogRows(t, h, "anon.example")
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].tenantID != nil {
		t.Fatalf("anonymous call should land tenant_id = NULL, got %s", rows[0].tenantID)
	}
}

func TestDNSResolutionLog_CHECKConstraint_RejectsBlockWithIP(t *testing.T) {
	// Defence in depth: the database CHECK constraint must reject any
	// row that tries to land decision='block' with a non-NULL
	// pinned_ip, even if a future writer forgets the in-app guard.
	h := startHarness(t)
	h.truncate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := h.pool.Exec(ctx, `
INSERT INTO dns_resolution_log
    (id, tenant_id, host, pinned_ip, verified_with_dnssec,
     decision, reason, phase, created_at)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8, $9)
`,
		uuid.New(), uuid.New(), "evil.example", "127.0.0.1", false,
		"block", "private_ip", "validate", time.Now().UTC(),
	)
	if err == nil {
		t.Fatalf("CHECK constraint must reject block + pinned_ip")
	}
	var pgErr interface{ SQLState() string }
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected pg constraint error, got %v", err)
	}
	// 23514 = check_violation
	if pgErr.SQLState() != "23514" {
		t.Fatalf("sqlstate = %s, want 23514", pgErr.SQLState())
	}
}

// mustAddr is a small helper so the tests can express a netip.Addr
// inline without erroring at every call site.
func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse addr %q: %v", s, err)
	}
	return a
}
