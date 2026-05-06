//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/webhook"
)

// TestTG4_CrossTenantIdempotencySegmentation — ADR §4 T-G4.
//
// The same raw_payload addressed to two distinct tenants on the same
// channel must produce two distinct webhook_idempotency rows and two
// distinct raw_event rows + publishes. The PK
// (tenant_id, channel, idempotency_key) is the safety net.
func TestTG4_CrossTenantIdempotencySegmentation(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)

	tenantA := mustParseTenant(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	tenantB := mustParseTenant(t, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	insertToken(t, h.pool, tenantA, "whatsapp", "tok-A", 0, time.Time{})
	insertToken(t, h.pool, tenantB, "whatsapp", "tok-B", 0, time.Time{})
	insertAssociation(t, h.pool, tenantA, "whatsapp", "PHONE-A")
	insertAssociation(t, h.pool, tenantB, "whatsapp", "PHONE-B")

	now := time.Now().UTC().Truncate(time.Second)
	st := newStack(t, h, now)

	// Each tenant's body carries *their own* phone_number_id — same Meta
	// shape but addressed to different tenants. The point of T-G4 is
	// that even if the bodies were byte-identical, the PK would
	// segment them. We use distinct phone_number_ids because the rev 3
	// cross-check (F-12) requires it; the segmentation invariant
	// applies independently.
	reqA := signedMetaRequest(t, "whatsapp", "tok-A", "PHONE-A", now)
	reqB := signedMetaRequest(t, "whatsapp", "tok-B", "PHONE-B", now)

	if r := st.svc.Handle(context.Background(), reqA); r.Outcome != webhook.OutcomeAccepted {
		t.Fatalf("A: Outcome=%s err=%v", r.Outcome, r.Err)
	}
	if r := st.svc.Handle(context.Background(), reqB); r.Outcome != webhook.OutcomeAccepted {
		t.Fatalf("B: Outcome=%s err=%v", r.Outcome, r.Err)
	}

	if got := countIdempotency(t, h.pool, tenantA, "whatsapp"); got != 1 {
		t.Errorf("idempotency(A) = %d, want 1", got)
	}
	if got := countIdempotency(t, h.pool, tenantB, "whatsapp"); got != 1 {
		t.Errorf("idempotency(B) = %d, want 1", got)
	}
	if got := countRawEvent(t, h.pool, tenantA, "whatsapp"); got != 1 {
		t.Errorf("raw_event(A) = %d, want 1", got)
	}
	if got := countRawEvent(t, h.pool, tenantB, "whatsapp"); got != 1 {
		t.Errorf("raw_event(B) = %d, want 1", got)
	}

	if calls := st.publisher.Calls(); len(calls) != 2 {
		t.Fatalf("publishes = %d, want 2", len(calls))
	}
	a, b := st.publisher.Calls()[0], st.publisher.Calls()[1]
	if a.TenantID == b.TenantID {
		t.Errorf("two publishes carried the same tenant id (%s)", a.TenantID)
	}
}

// TestTG5_ReplayWindowViolation — ADR §4 T-G5 (rev 3).
//
// Regression gate against future widening of `Config.PastWindow` (default
// 5 min per ADR §2 D3). A request signed and authenticated correctly but
// carrying a payload timestamp 25 hours in the past must be dropped at
// the timestamp-window check — *before* the idempotency INSERT and the
// raw_event INSERT — and must not publish.
//
// This is intentionally distinct from T-1 (replay): T-1 asserts the
// idempotency PK conflict path (`OutcomeReplay`); T-G5 asserts the
// timestamp-out-of-range path (`OutcomeReplayWindowViolation`). If a
// future change collapsed both into one outcome — or widened PastWindow
// past 25h — this test would fail.
func TestTG5_ReplayWindowViolation(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)

	tenant := mustParseTenant(t, "55555555-5555-5555-5555-555555555555")
	insertToken(t, h.pool, tenant, "whatsapp", "tok-G5", 0, time.Time{})
	insertAssociation(t, h.pool, tenant, "whatsapp", "PHONE-G5")

	now := time.Now().UTC().Truncate(time.Second)
	st := newStack(t, h, now)

	// Body timestamp 25 hours in the past — far beyond any plausible
	// PastWindow widening.
	stale := now.Add(-25 * time.Hour)
	req := signedMetaRequest(t, "whatsapp", "tok-G5", "PHONE-G5", stale)

	res := st.svc.Handle(context.Background(), req)
	if res.Outcome != webhook.OutcomeReplayWindowViolation {
		t.Fatalf("Outcome = %s, want replay_window_violation (err=%v)", res.Outcome, res.Err)
	}

	// Window violation drops *before* idempotency INSERT — the row must
	// not exist. A regression that widened PastWindow past 25h would let
	// this row get written and OutcomeReplay would surface on a retry,
	// not OutcomeReplayWindowViolation.
	if got := countIdempotency(t, h.pool, tenant, "whatsapp"); got != 0 {
		t.Errorf("webhook_idempotency rows = %d, want 0 (window violation must NOT insert)", got)
	}
	if got := countRawEvent(t, h.pool, tenant, "whatsapp"); got != 0 {
		t.Errorf("raw_event rows = %d, want 0", got)
	}
	if calls := st.publisher.Calls(); len(calls) != 0 {
		t.Errorf("publisher.Calls = %d, want 0 for window violation", len(calls))
	}
}

// TestTG6_TokenRotationOverlap — ADR §4 T-G6.
//
// When a token rotates with overlap_minutes > 0, both the old (revoked
// in the future) and the new ("permanently active") rows coexist for
// the grace window. The lookup query orders by (revoked_at IS NULL DESC,
// created_at DESC), so the *new* row wins for a freshly-issued token,
// but a request signed with the OLD token's plaintext still resolves
// during the grace window.
func TestTG6_TokenRotationOverlap(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)

	tenant := mustParseTenant(t, "66666666-6666-6666-6666-666666666666")
	now := time.Now().UTC().Truncate(time.Second)

	// Old token: scheduled to revoke 5 minutes from now.
	insertToken(t, h.pool, tenant, "whatsapp", "tok-old", 5, now.Add(5*time.Minute))
	// New token: permanently active.
	insertToken(t, h.pool, tenant, "whatsapp", "tok-new", 0, time.Time{})
	insertAssociation(t, h.pool, tenant, "whatsapp", "PHONE-T6")

	// Both rows must coexist.
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM webhook_tokens WHERE tenant_id = $1 AND channel = 'whatsapp'`,
		tenant[:]).Scan(&n); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if n != 2 {
		t.Fatalf("token rows = %d, want 2 (rotation overlap)", n)
	}

	st := newStack(t, h, now)

	// Request signed via OLD token: still valid (now < revoked_at).
	reqOld := signedMetaRequest(t, "whatsapp", "tok-old", "PHONE-T6", now)
	if r := st.svc.Handle(context.Background(), reqOld); r.Outcome != webhook.OutcomeAccepted {
		t.Fatalf("old token within grace: Outcome=%s err=%v", r.Outcome, r.Err)
	}
	if r := st.svc.Handle(context.Background(), reqOld); r.TenantID != tenant {
		t.Errorf("old token resolved tenant = %s, want %s", r.TenantID, tenant)
	}

	// Request signed via NEW token: also valid.
	reqNew := signedMetaRequest(t, "whatsapp", "tok-new", "PHONE-T6", now.Add(time.Second))
	if r := st.svc.Handle(context.Background(), reqNew); r.Outcome != webhook.OutcomeAccepted {
		t.Fatalf("new token: Outcome=%s err=%v", r.Outcome, r.Err)
	}

	// Advance clock past the grace window — old token must now drop.
	st.clock.Set(now.Add(6 * time.Minute))
	expired := signedMetaRequest(t, "whatsapp", "tok-old", "PHONE-T6", now.Add(6*time.Minute))
	if r := st.svc.Handle(context.Background(), expired); r.Outcome != webhook.OutcomeRevokedToken {
		t.Fatalf("old token after grace: Outcome=%s, want revoked_token", r.Outcome)
	}
}

// TestTG8_TokenAtRestIsHashBytea — ADR §4 T-G8.
//
// Confirms that webhook_tokens.token_hash is a BYTEA column and that
// the stored value equals sha256(plaintext). Plaintext is never stored
// or recoverable from the row.
func TestTG8_TokenAtRestIsHashBytea(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)

	tenant := mustParseTenant(t, "88888888-8888-8888-8888-888888888888")
	plaintext := "tok-G8-secret"
	insertToken(t, h.pool, tenant, "whatsapp", plaintext, 0, time.Time{})

	// 1. Column type must be bytea.
	var dataType string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT data_type FROM information_schema.columns
		 WHERE table_name='webhook_tokens' AND column_name='token_hash'`).
		Scan(&dataType); err != nil {
		t.Fatalf("information_schema: %v", err)
	}
	if dataType != "bytea" {
		t.Errorf("token_hash data_type = %q, want bytea", dataType)
	}

	// 2. Stored value equals sha256(plaintext); plaintext absent.
	var storedHash []byte
	if err := h.pool.QueryRow(context.Background(),
		`SELECT token_hash FROM webhook_tokens WHERE tenant_id = $1`,
		tenant[:]).Scan(&storedHash); err != nil {
		t.Fatalf("scan stored hash: %v", err)
	}
	want := sha256.Sum256([]byte(plaintext))
	if len(storedHash) != len(want) {
		t.Fatalf("stored hash len = %d, want %d", len(storedHash), len(want))
	}
	for i := range storedHash {
		if storedHash[i] != want[i] {
			t.Fatalf("stored hash[%d] differs", i)
		}
	}

	// 3. No column on the table holds the plaintext literal.
	rows, err := h.pool.Query(context.Background(),
		`SELECT column_name FROM information_schema.columns WHERE table_name='webhook_tokens'`)
	if err != nil {
		t.Fatalf("columns query: %v", err)
	}
	defer rows.Close()
	plaintextSeen := false
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatalf("scan col: %v", err)
		}
		// Schema names with "token" but never "plaintext" or "secret".
		if col == "token" || col == "token_plaintext" || col == "secret" {
			plaintextSeen = true
		}
	}
	if plaintextSeen {
		t.Errorf("webhook_tokens leaked a plaintext column")
	}
}

// TestTG9_CrossTenantBodyMisroutingDropsSilently — ADR §4 T-G9.
//
// An attacker captures a Meta payload addressed to tenant A (with a
// valid HMAC) and POSTs it to tenant B's URL/token. The service must
// detect the tenant_body_mismatch and drop: zero entries in
// webhook_idempotency for B and zero publishes.
func TestTG9_CrossTenantBodyMisroutingDropsSilently(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)

	tenantA := mustParseTenant(t, "a9a9a9a9-a9a9-a9a9-a9a9-a9a9a9a9a9a9")
	tenantB := mustParseTenant(t, "b9b9b9b9-b9b9-b9b9-b9b9-b9b9b9b9b9b9")

	insertToken(t, h.pool, tenantA, "whatsapp", "tok-A9", 0, time.Time{})
	insertToken(t, h.pool, tenantB, "whatsapp", "tok-B9", 0, time.Time{})
	insertAssociation(t, h.pool, tenantA, "whatsapp", "PHONE-A9")
	insertAssociation(t, h.pool, tenantB, "whatsapp", "PHONE-B9")

	now := time.Now().UTC().Truncate(time.Second)
	st := newStack(t, h, now)

	// Body addressed to A (PHONE-A9) but POSTed to B's token. HMAC
	// passes (single app secret), token lookup resolves to tenant B,
	// but cross-check sees PHONE-A9 ≠ tenant B's association.
	misrouted := signedMetaRequest(t, "whatsapp", "tok-B9", "PHONE-A9", now)

	res := st.svc.Handle(context.Background(), misrouted)
	if res.Outcome != webhook.OutcomeTenantBodyMismatch {
		t.Fatalf("Outcome = %s, want tenant_body_mismatch (err=%v)", res.Outcome, res.Err)
	}
	if res.TenantID != tenantB {
		// The metric is allowed to label with the URL-resolved tenant
		// (the *legitimate* destination, even though the body is
		// invalid). See ADR §5.
		t.Errorf("Result.TenantID = %s, want %s", res.TenantID, tenantB)
	}

	if got := countIdempotency(t, h.pool, tenantB, "whatsapp"); got != 0 {
		t.Errorf("idempotency(B) = %d, want 0 (misroute must NOT insert)", got)
	}
	if got := countRawEvent(t, h.pool, tenantB, "whatsapp"); got != 0 {
		t.Errorf("raw_event(B) = %d, want 0", got)
	}
	if calls := st.publisher.Calls(); len(calls) != 0 {
		t.Errorf("publisher.Calls = %d, want 0 for misroute", len(calls))
	}
}
