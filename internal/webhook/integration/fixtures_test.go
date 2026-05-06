//go:build integration

package integration_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/channel/meta"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	"github.com/pericles-luz/crm/internal/webhook"
)

// metaAppSecret is the test-only Meta app secret; HMAC-SHA-256 over the
// raw body with this key produces the X-Hub-Signature-256 the adapter
// expects.
const metaAppSecret = "integration-app-secret"

// stack bundles the production webhook adapters wired against the
// harness's pgxpool, with deterministic test-only Clock + capture sinks
// for the publisher. Used by every scenario test.
type stack struct {
	svc          *webhook.Service
	publisher    *capturingPublisher
	clock        *fixedClock
	tokens       *pgstore.TokenStore
	idem         *pgstore.IdempotencyStore
	rawEvents    *pgstore.RawEventStore
	associations *pgstore.TenantAssociationStore
	metrics      *captureMetrics
}

// newStack wires the webhook.Service against real Postgres adapters
// using the harness pool. The async publish runner is replaced with a
// synchronous one so tests can assert publish state without flaky
// `time.Sleep` calls.
func newStack(t *testing.T, h *harness, now time.Time) *stack {
	t.Helper()

	adapter, err := meta.New("whatsapp", metaAppSecret)
	if err != nil {
		t.Fatalf("meta.New: %v", err)
	}

	publisher := &capturingPublisher{}
	clock := &fixedClock{t: now}
	metrics := &captureMetrics{}

	tokens := pgstore.NewTokenStore(h.pool)
	idem := pgstore.NewIdempotencyStore(h.pool)
	rawEvents := pgstore.NewRawEventStore(h.pool)
	associations := pgstore.NewTenantAssociationStore(h.pool)

	svc, err := webhook.NewService(webhook.Config{
		Adapters:               []webhook.ChannelAdapter{adapter},
		TokenStore:             tokens,
		IdempotencyStore:       idem,
		RawEventStore:          rawEvents,
		Publisher:              publisher,
		TenantAssociationStore: associations,
		Clock:                  clock,
		Metrics:                metrics,
		// Synchronous async runner so tests assert publish + MarkPublished
		// state without waiting on a goroutine.
		AsyncRunner: func(f func()) { f() },
	})
	if err != nil {
		t.Fatalf("webhook.NewService: %v", err)
	}
	return &stack{
		svc:          svc,
		publisher:    publisher,
		clock:        clock,
		tokens:       tokens,
		idem:         idem,
		rawEvents:    rawEvents,
		associations: associations,
		metrics:      metrics,
	}
}

// fixedClock returns a constant time. Tests advance it via Set.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fixedClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// capturingPublisher records every Publish call so tests can assert
// what was published (or that nothing was).
type capturingPublisher struct {
	mu    sync.Mutex
	calls []publishCall
	err   error
}

type publishCall struct {
	EventID  [16]byte
	TenantID webhook.TenantID
	Channel  string
	Payload  []byte
	Headers  map[string][]string
}

func (p *capturingPublisher) Publish(_ context.Context, eventID [16]byte, tenantID webhook.TenantID, channel string, payload []byte, headers map[string][]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	clone := make([]byte, len(payload))
	copy(clone, payload)
	p.calls = append(p.calls, publishCall{
		EventID:  eventID,
		TenantID: tenantID,
		Channel:  channel,
		Payload:  clone,
		Headers:  cloneHeaders(headers),
	})
	return p.err
}

func (p *capturingPublisher) Calls() []publishCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]publishCall, len(p.calls))
	copy(out, p.calls)
	return out
}

func cloneHeaders(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for k, v := range in {
		clone := make([]string, len(v))
		copy(clone, v)
		out[k] = clone
	}
	return out
}

// captureMetrics records every metric call so the unknown_token /
// revoked_token outcomes can be asserted by label.
type captureMetrics struct {
	mu       sync.Mutex
	received []receivedCall
	conflict []conflictCall
}

type receivedCall struct {
	Channel   string
	Outcome   webhook.Outcome
	TenantID  webhook.TenantID
	HasTenant bool
}

type conflictCall struct {
	Channel  string
	TenantID webhook.TenantID
}

func (m *captureMetrics) IncReceived(channel string, outcome webhook.Outcome, tenantID webhook.TenantID, hasTenant bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.received = append(m.received, receivedCall{Channel: channel, Outcome: outcome, TenantID: tenantID, HasTenant: hasTenant})
}

func (m *captureMetrics) ObserveAck(string, time.Duration) {}

func (m *captureMetrics) IncIdempotencyConflict(channel string, tenantID webhook.TenantID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conflict = append(m.conflict, conflictCall{Channel: channel, TenantID: tenantID})
}

func (m *captureMetrics) ReceivedOutcomes() []webhook.Outcome {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]webhook.Outcome, len(m.received))
	for i, c := range m.received {
		out[i] = c.Outcome
	}
	return out
}

func (m *captureMetrics) Received() []receivedCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]receivedCall, len(m.received))
	copy(out, m.received)
	return out
}

// insertToken hashes the plaintext, INSERTs a webhook_tokens row, and
// returns the plaintext (unchanged) for the test to use. revokedAt may
// be the zero time to leave the row "permanently active".
func insertToken(t *testing.T, pool *pgxpool.Pool, tenantID webhook.TenantID, channel, plaintext string, overlapMinutes int, revokedAt time.Time) {
	t.Helper()
	hash := sha256.Sum256([]byte(plaintext))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if revokedAt.IsZero() {
		_, err := pool.Exec(ctx,
			`INSERT INTO webhook_tokens (tenant_id, channel, token_hash, overlap_minutes) VALUES ($1, $2, $3, $4)`,
			tenantID[:], channel, hash[:], overlapMinutes,
		)
		if err != nil {
			t.Fatalf("insert token: %v", err)
		}
		return
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO webhook_tokens (tenant_id, channel, token_hash, overlap_minutes, revoked_at) VALUES ($1, $2, $3, $4, $5)`,
		tenantID[:], channel, hash[:], overlapMinutes, revokedAt,
	)
	if err != nil {
		t.Fatalf("insert revoked token: %v", err)
	}
}

// insertAssociation registers (channel, association) → tenant in
// tenant_channel_associations.
func insertAssociation(t *testing.T, pool *pgxpool.Pool, tenantID webhook.TenantID, channel, association string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`INSERT INTO tenant_channel_associations (tenant_id, channel, association) VALUES ($1, $2, $3)`,
		tenantID[:], channel, association,
	)
	if err != nil {
		t.Fatalf("insert association: %v", err)
	}
}

// signedMetaRequest builds a webhook.Request with a Meta-shape body
// signed with metaAppSecret. phoneNumberID is what BodyTenantAssociation
// will return — used both for legitimate and cross-tenant misroute
// fixtures.
func signedMetaRequest(t *testing.T, channel, token string, phoneNumberID string, ts time.Time) webhook.Request {
	t.Helper()
	body := []byte(fmt.Sprintf(
		`{"entry":[{"id":"E1","time":%d,"changes":[{"value":{"metadata":{"phone_number_id":%q}}}]}]}`,
		ts.Unix(), phoneNumberID,
	))
	mac := hmac.New(sha256.New, []byte(metaAppSecret))
	_, _ = mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return webhook.Request{
		Channel:   channel,
		Token:     token,
		Body:      body,
		Headers:   map[string][]string{"X-Hub-Signature-256": {sig}},
		RequestID: fmt.Sprintf("test-req-%d", ts.UnixNano()),
	}
}

// mustParseTenant decodes a canonical UUID into webhook.TenantID. Used
// for readable test fixtures.
func mustParseTenant(t *testing.T, s string) webhook.TenantID {
	t.Helper()
	tid, err := webhook.ParseTenantID(s)
	if err != nil {
		t.Fatalf("ParseTenantID(%q): %v", s, err)
	}
	return tid
}

// countIdempotency returns the number of webhook_idempotency rows
// matching (tenantID, channel). Used by replay/cross-tenant tests.
func countIdempotency(t *testing.T, pool *pgxpool.Pool, tenantID webhook.TenantID, channel string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM webhook_idempotency WHERE tenant_id = $1 AND channel = $2`,
		tenantID[:], channel,
	).Scan(&n)
	if err != nil {
		t.Fatalf("count idempotency: %v", err)
	}
	return n
}

// countRawEvent returns the number of raw_event rows matching
// (tenantID, channel).
func countRawEvent(t *testing.T, pool *pgxpool.Pool, tenantID webhook.TenantID, channel string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM raw_event WHERE tenant_id = $1 AND channel = $2`,
		tenantID[:], channel,
	).Scan(&n)
	if err != nil {
		t.Fatalf("count raw_event: %v", err)
	}
	return n
}
