//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/webhook"
)

// TestT1_ReplayHitsIdempotencyConflict — ADR §4 T-1.
//
// Same payload signed and POSTed twice: first writes one row to
// webhook_idempotency + raw_event and triggers a publish; second hits
// ON CONFLICT inside the INSERT, returns OutcomeReplay, and produces
// no second publish + no second raw_event row.
func TestT1_ReplayHitsIdempotencyConflict(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)

	tenant := mustParseTenant(t, "11111111-1111-1111-1111-111111111111")
	insertToken(t, h.pool, tenant, "whatsapp", "tok-T1", 0, time.Time{})
	insertAssociation(t, h.pool, tenant, "whatsapp", "PHONE-T1")

	now := time.Now().UTC().Truncate(time.Second)
	st := newStack(t, h, now)
	req := signedMetaRequest(t, "whatsapp", "tok-T1", "PHONE-T1", now)

	ctx := context.Background()

	first := st.svc.Handle(ctx, req)
	if first.Outcome != webhook.OutcomeAccepted {
		t.Fatalf("first.Outcome = %s, want accepted (err=%v)", first.Outcome, first.Err)
	}

	second := st.svc.Handle(ctx, req)
	if second.Outcome != webhook.OutcomeReplay {
		t.Fatalf("second.Outcome = %s, want replay (err=%v)", second.Outcome, second.Err)
	}

	if got := countIdempotency(t, h.pool, tenant, "whatsapp"); got != 1 {
		t.Errorf("webhook_idempotency rows = %d, want 1", got)
	}
	if got := countRawEvent(t, h.pool, tenant, "whatsapp"); got != 1 {
		t.Errorf("raw_event rows = %d, want 1 (replay must NOT insert)", got)
	}

	if calls := st.publisher.Calls(); len(calls) != 1 {
		t.Errorf("publisher.Calls = %d, want exactly 1 (replay must not publish)", len(calls))
	}
}

// TestT3_UnknownTokenSilentDropMetric — ADR §4 T-3.
//
// POST against a token that does not exist in webhook_tokens. The
// service must drop silently (no idempotency row, no raw_event row, no
// publish) and emit a metric with outcome=unknown_token.
func TestT3_UnknownTokenSilentDropMetric(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)

	now := time.Now().UTC().Truncate(time.Second)
	st := newStack(t, h, now)
	req := signedMetaRequest(t, "whatsapp", "totally-unknown", "PHONE-X", now)

	res := st.svc.Handle(context.Background(), req)
	if res.Outcome != webhook.OutcomeUnknownToken {
		t.Fatalf("Outcome = %s, want unknown_token (err=%v)", res.Outcome, res.Err)
	}

	// No tenant resolved → no scoped count is meaningful, but the
	// global counts must be zero.
	var idemRows int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM webhook_idempotency`).Scan(&idemRows); err != nil {
		t.Fatalf("count idem: %v", err)
	}
	if idemRows != 0 {
		t.Errorf("webhook_idempotency rows = %d, want 0", idemRows)
	}

	var rawRows int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM raw_event`).Scan(&rawRows); err != nil {
		t.Fatalf("count raw_event: %v", err)
	}
	if rawRows != 0 {
		t.Errorf("raw_event rows = %d, want 0", rawRows)
	}

	if calls := st.publisher.Calls(); len(calls) != 0 {
		t.Errorf("publisher.Calls = %d, want 0", len(calls))
	}

	// Metric outcome must be unknown_token, with no tenant label.
	if outcomes := st.metrics.ReceivedOutcomes(); len(outcomes) != 1 || outcomes[0] != webhook.OutcomeUnknownToken {
		t.Errorf("metric outcomes = %v, want [unknown_token]", outcomes)
	}
	if calls := st.metrics.Received(); len(calls) == 1 && calls[0].HasTenant {
		t.Errorf("unknown_token metric must NOT carry tenant label, got %+v", calls[0])
	}
}

// TestT7_RevokedTokenDropAfterGrace — ADR §4 T-7.
//
// `revoked_at` semantics (rev 3 / F-13): the row stays valid until
// now() < revoked_at; once now is at-or-after revoked_at, the lookup
// returns ErrTokenRevoked and the request is dropped.
func TestT7_RevokedTokenDropAfterGrace(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)

	tenant := mustParseTenant(t, "77777777-7777-7777-7777-777777777777")
	now := time.Now().UTC().Truncate(time.Second)
	// Revoked five seconds ago — past the cut-over.
	revokedAt := now.Add(-5 * time.Second)
	insertToken(t, h.pool, tenant, "whatsapp", "tok-T7", 0, revokedAt)

	st := newStack(t, h, now)
	req := signedMetaRequest(t, "whatsapp", "tok-T7", "PHONE-T7", now)

	res := st.svc.Handle(context.Background(), req)
	if res.Outcome != webhook.OutcomeRevokedToken {
		t.Fatalf("Outcome = %s, want revoked_token (err=%v)", res.Outcome, res.Err)
	}

	if got := countIdempotency(t, h.pool, tenant, "whatsapp"); got != 0 {
		t.Errorf("webhook_idempotency rows = %d, want 0", got)
	}
	if got := countRawEvent(t, h.pool, tenant, "whatsapp"); got != 0 {
		t.Errorf("raw_event rows = %d, want 0", got)
	}
	if calls := st.publisher.Calls(); len(calls) != 0 {
		t.Errorf("publisher.Calls = %d, want 0", len(calls))
	}
}
