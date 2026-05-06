package webhook_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/webhook"
)

// fakeAdapter is a configurable webhook.ChannelAdapter for table-driven
// tests. By default it simulates an AppLevel adapter that accepts every
// signature; individual tests override the closures to drive specific
// branches.
type fakeAdapter struct {
	name        string
	scope       webhook.SecretScope
	verifyApp   func(ctx context.Context, body []byte, h map[string][]string) error
	verifyTen   func(ctx context.Context, t webhook.TenantID, body []byte, h map[string][]string) error
	extract     func(h map[string][]string, body []byte) (time.Time, error)
	parse       func(body []byte) (webhook.Event, error)
	bodyAssoc   func(body []byte) (string, bool)
	tenantSeen  []webhook.TenantID // captured by VerifyTenant for T-G2
	preTenantOk bool
}

func (f *fakeAdapter) Name() string                     { return f.name }
func (f *fakeAdapter) SecretScope() webhook.SecretScope { return f.scope }
func (f *fakeAdapter) VerifyApp(c context.Context, b []byte, h map[string][]string) error {
	if f.verifyApp != nil {
		return f.verifyApp(c, b, h)
	}
	return nil
}
func (f *fakeAdapter) VerifyTenant(c context.Context, t webhook.TenantID, b []byte, h map[string][]string) error {
	f.tenantSeen = append(f.tenantSeen, t)
	if _, ok := webhook.AuthenticatedTenantID(c); ok {
		f.preTenantOk = true
	}
	if f.verifyTen != nil {
		return f.verifyTen(c, t, b, h)
	}
	return nil
}
func (f *fakeAdapter) ExtractTimestamp(h map[string][]string, b []byte) (time.Time, error) {
	if f.extract != nil {
		return f.extract(h, b)
	}
	return time.Unix(1_700_000_000, 0).UTC(), nil
}
func (f *fakeAdapter) ParseEvent(b []byte) (webhook.Event, error) {
	if f.parse != nil {
		return f.parse(b)
	}
	return webhook.Event{}, nil
}
func (f *fakeAdapter) BodyTenantAssociation(b []byte) (string, bool) {
	if f.bodyAssoc != nil {
		return f.bodyAssoc(b)
	}
	return "", false // default: skip cross-check (no association in body)
}

// fakeTokenStore returns the configured tenantID for any (channel, hash).
type fakeTokenStore struct {
	tenant    webhook.TenantID
	err       error
	markErr   error
	calls     int
	markCalls int
}

func (s *fakeTokenStore) Lookup(_ context.Context, _ string, _ []byte, _ time.Time) (webhook.TenantID, error) {
	s.calls++
	return s.tenant, s.err
}
func (s *fakeTokenStore) MarkUsed(_ context.Context, _ string, _ []byte, _ time.Time) error {
	s.markCalls++
	return s.markErr
}

// fakeIdem records inserts and returns false for any key it has seen.
type fakeIdem struct {
	mu   sync.Mutex
	seen map[[32]byte]bool
	err  error
}

func newFakeIdem() *fakeIdem { return &fakeIdem{seen: map[[32]byte]bool{}} }
func (f *fakeIdem) CheckAndStore(_ context.Context, _ webhook.TenantID, _ string, key []byte, _ time.Time) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var k [32]byte
	copy(k[:], key)
	if f.seen[k] {
		return false, nil
	}
	f.seen[k] = true
	return true, nil
}

// fakeRawEventStore counts inserts/marks; returns a deterministic id.
type fakeRawEventStore struct {
	mu        sync.Mutex
	rows      []webhook.RawEventRow
	id        [16]byte
	insertErr error
	markErr   error
	marked    int
}

func (s *fakeRawEventStore) Insert(_ context.Context, row webhook.RawEventRow) ([16]byte, error) {
	if s.insertErr != nil {
		return [16]byte{}, s.insertErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, row)
	s.id[15]++
	return s.id, nil
}
func (s *fakeRawEventStore) MarkPublished(_ context.Context, _ [16]byte, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marked++
	return s.markErr
}

// fakePublisher records calls and forces synchronous semantics through
// service config (AsyncRunner returns immediately).
type fakePublisher struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (p *fakePublisher) Publish(_ context.Context, _ [16]byte, _ webhook.TenantID, _ string, _ []byte, _ map[string][]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.err
}

// fakeAssoc allows-by-default; tests can switch to deny or to a
// per-tuple ruleset for T-G9.
type fakeAssoc struct {
	allow func(tenantID webhook.TenantID, channel, association string) bool
	err   error
	calls int
}

func (a *fakeAssoc) CheckAssociation(_ context.Context, t webhook.TenantID, channel, association string) (bool, error) {
	a.calls++
	if a.err != nil {
		return false, a.err
	}
	if a.allow == nil {
		return true, nil
	}
	return a.allow(t, channel, association), nil
}

// fakeMetrics + fakeLogger record calls so we can assert on labels.
type fakeMetrics struct {
	mu                sync.Mutex
	receivedTenants   []webhook.TenantID
	receivedHasTenant []bool
	receivedOutcomes  []webhook.Outcome
	idemConflicts     int
}

func (m *fakeMetrics) IncReceived(_ string, o webhook.Outcome, t webhook.TenantID, has bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.receivedOutcomes = append(m.receivedOutcomes, o)
	m.receivedTenants = append(m.receivedTenants, t)
	m.receivedHasTenant = append(m.receivedHasTenant, has)
}
func (m *fakeMetrics) ObserveAck(string, time.Duration) {}
func (m *fakeMetrics) IncIdempotencyConflict(string, webhook.TenantID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.idemConflicts++
}

type fakeLogger struct {
	mu      sync.Mutex
	records []webhook.LogRecord
}

func (l *fakeLogger) LogResult(_ context.Context, r webhook.LogRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r)
}

func newServiceUnderTest(t *testing.T, adapters []webhook.ChannelAdapter, opts ...func(*webhook.Config)) (*webhook.Service, *fakeIdem, *fakeRawEventStore, *fakePublisher, *fakeTokenStore, *fakeMetrics, *fakeLogger) {
	t.Helper()
	idem := newFakeIdem()
	raw := &fakeRawEventStore{}
	pub := &fakePublisher{}
	tokens := &fakeTokenStore{tenant: webhook.TenantID{0xaa}}
	assoc := &fakeAssoc{}
	metrics := &fakeMetrics{}
	logger := &fakeLogger{}
	cfg := webhook.Config{
		Adapters:               adapters,
		TokenStore:             tokens,
		IdempotencyStore:       idem,
		RawEventStore:          raw,
		Publisher:              pub,
		TenantAssociationStore: assoc,
		Clock:                  fixedClock{t: time.Unix(1_700_000_000, 0).UTC()},
		Metrics:                metrics,
		Logger:                 logger,
		AsyncRunner:            func(f func()) { f() },
		PublishContext:         context.Background,
	}
	for _, o := range opts {
		o(&cfg)
	}
	svc, err := webhook.NewService(cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, idem, raw, pub, tokens, metrics, logger
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func metaPayloadAt(ts int64) []byte {
	return []byte(`{"entry":[{"id":"123","time":` + itoa(ts) + `}]}`)
}

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	negative := false
	if i < 0 {
		negative = true
		i = -i
	}
	var b [21]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if negative {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// --- T-1: replay imediato → segundo POST não publica.
func TestService_ReplayBlocked(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{name: "whatsapp", scope: webhook.SecretScopeApp}
	svc, idem, raw, pub, _, metrics, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})

	body := metaPayloadAt(1_700_000_000)
	req := webhook.Request{Channel: "whatsapp", Token: "t", Body: body, Headers: map[string][]string{}}

	first := svc.Handle(context.Background(), req)
	if first.Outcome != webhook.OutcomeAccepted {
		t.Fatalf("first outcome = %s, want accepted", first.Outcome)
	}
	second := svc.Handle(context.Background(), req)
	if second.Outcome != webhook.OutcomeReplay {
		t.Fatalf("second outcome = %s, want replay", second.Outcome)
	}
	if got := len(idem.seen); got != 1 {
		t.Fatalf("idempotency rows = %d, want 1", got)
	}
	if got := len(raw.rows); got != 1 {
		t.Fatalf("raw_event rows = %d, want 1", got)
	}
	if pub.calls != 1 {
		t.Fatalf("publish calls = %d, want 1", pub.calls)
	}
	if metrics.idemConflicts != 1 {
		t.Fatalf("idempotency conflicts = %d, want 1", metrics.idemConflicts)
	}
}

// --- T-2: timestamp window violation → 200 + drop.
func TestService_TimestampWindowViolation(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{
		name:  "whatsapp",
		scope: webhook.SecretScopeApp,
		extract: func(_ map[string][]string, _ []byte) (time.Time, error) {
			return time.Unix(1_699_990_000, 0).UTC(), nil // 6+ min in past
		},
	}
	svc, _, raw, pub, _, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})

	res := svc.Handle(context.Background(), webhook.Request{Channel: "whatsapp", Token: "t", Body: []byte(`{}`)})
	if res.Outcome != webhook.OutcomeReplayWindowViolation {
		t.Fatalf("outcome = %s, want replay_window_violation", res.Outcome)
	}
	if len(raw.rows) != 0 || pub.calls != 0 {
		t.Fatalf("no rows or publishes expected on window violation")
	}
}

// --- T-3: token desconhecido → 200 silencioso, no rows.
func TestService_UnknownToken(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{name: "whatsapp", scope: webhook.SecretScopeApp}
	svc, _, raw, pub, tokens, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})
	tokens.err = webhook.ErrTokenUnknown

	res := svc.Handle(context.Background(), webhook.Request{Channel: "whatsapp", Token: "x", Body: []byte(`{}`)})
	if res.Outcome != webhook.OutcomeUnknownToken {
		t.Fatalf("outcome = %s, want unknown_token", res.Outcome)
	}
	if len(raw.rows) != 0 || pub.calls != 0 {
		t.Fatal("no insert/publish expected")
	}
}

// --- T-4: HMAC inválido → 200 silencioso.
func TestService_HMACInvalid(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{
		name:      "whatsapp",
		scope:     webhook.SecretScopeApp,
		verifyApp: func(context.Context, []byte, map[string][]string) error { return webhook.ErrSignatureInvalid },
	}
	svc, _, raw, pub, tokens, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})

	res := svc.Handle(context.Background(), webhook.Request{Channel: "whatsapp", Token: "t", Body: []byte(`{}`)})
	if res.Outcome != webhook.OutcomeSignatureInvalid {
		t.Fatalf("outcome = %s, want signature_invalid", res.Outcome)
	}
	// HMAC invalid for an AppLevel adapter must run *before* TokenStore.
	if tokens.calls != 0 {
		t.Fatalf("TokenStore.Lookup called %d times before HMAC for AppLevel", tokens.calls)
	}
	if len(raw.rows) != 0 || pub.calls != 0 {
		t.Fatal("no insert/publish expected")
	}
}

// --- T-5: adapter contract inválido fail-fast no startup.
func TestService_InvalidAdapterContract(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		adapter webhook.ChannelAdapter
		wantSub string
	}{
		{"empty name", &fakeAdapter{name: "", scope: webhook.SecretScopeApp}, "empty"},
		{"colon in name", &fakeAdapter{name: "bad:name", scope: webhook.SecretScopeApp}, "[a-z0-9_]+"},
		{"unknown scope", &fakeAdapter{name: "x", scope: webhook.SecretScopeUnknown}, "SecretScope"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := webhook.NewService(webhook.Config{
				Adapters:               []webhook.ChannelAdapter{tc.adapter},
				TokenStore:             &fakeTokenStore{},
				IdempotencyStore:       newFakeIdem(),
				RawEventStore:          &fakeRawEventStore{},
				Publisher:              &fakePublisher{},
				TenantAssociationStore: &fakeAssoc{},
			})
			if err == nil {
				t.Fatalf("NewService returned nil error for %s", tc.name)
			}
			if !contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err, tc.wantSub)
			}
		})
	}
}

// --- T-6: AppLevel HMAC verifies before TokenStore (defensive ordering).
func TestService_AppLevel_VerifyBeforeTokenStore(t *testing.T) {
	t.Parallel()
	verifyCount := 0
	adapter := &fakeAdapter{
		name:      "whatsapp",
		scope:     webhook.SecretScopeApp,
		verifyApp: func(context.Context, []byte, map[string][]string) error { verifyCount++; return nil },
	}
	svc, _, _, _, tokens, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})
	if _, err := webhook.ParseTenantID("00000000-0000-0000-0000-0000000000aa"); err != nil {
		t.Fatalf("ParseTenantID: %v", err)
	}

	res := svc.Handle(context.Background(), webhook.Request{Channel: "whatsapp", Token: "t", Body: metaPayloadAt(1_700_000_000)})
	if res.Outcome != webhook.OutcomeAccepted {
		t.Fatalf("outcome = %s, want accepted", res.Outcome)
	}
	if verifyCount != 1 || tokens.calls != 1 {
		t.Fatalf("verify=%d tokens=%d, want both 1", verifyCount, tokens.calls)
	}
}

// --- T-7: token revogado → 200 silencioso + outcome.
func TestService_RevokedToken(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{name: "whatsapp", scope: webhook.SecretScopeApp}
	svc, _, raw, pub, tokens, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})
	tokens.err = webhook.ErrTokenRevoked

	res := svc.Handle(context.Background(), webhook.Request{Channel: "whatsapp", Token: "t", Body: []byte(`{}`)})
	if res.Outcome != webhook.OutcomeRevokedToken {
		t.Fatalf("outcome = %s, want revoked_token", res.Outcome)
	}
	if len(raw.rows) != 0 || pub.calls != 0 {
		t.Fatal("no insert/publish expected")
	}
}

// --- T-G2 (TenantLevel): pre-HMAC path does NOT carry authenticated
//
//	tenant in ctx, and passes the claim tenant id to VerifyTenant.
func TestService_TenantLevel_ClaimNotAuthenticated(t *testing.T) {
	t.Parallel()
	tenant := webhook.TenantID{0xbb}
	adapter := &fakeAdapter{
		name:  "psp",
		scope: webhook.SecretScopeTenant,
		verifyTen: func(c context.Context, _ webhook.TenantID, _ []byte, _ map[string][]string) error {
			if _, ok := webhook.AuthenticatedTenantID(c); ok {
				t.Fatal("claim tenant must not be present in ctx pre-HMAC")
			}
			return nil
		},
	}
	svc, _, _, _, tokens, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})
	tokens.tenant = tenant

	res := svc.Handle(context.Background(), webhook.Request{Channel: "psp", Token: "t", Body: metaPayloadAt(1_700_000_000)})
	if res.Outcome != webhook.OutcomeAccepted {
		t.Fatalf("outcome = %s, want accepted", res.Outcome)
	}
	if got := len(adapter.tenantSeen); got != 1 || adapter.tenantSeen[0] != tenant {
		t.Fatalf("VerifyTenant tenantSeen = %+v, want [%v]", adapter.tenantSeen, tenant)
	}
}

// --- T-G3: timestamp missing (no entry[].time) → 200 + drop.
func TestService_TimestampMissing(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{
		name:  "whatsapp",
		scope: webhook.SecretScopeApp,
		extract: func(_ map[string][]string, _ []byte) (time.Time, error) {
			return time.Time{}, webhook.ErrTimestampMissing
		},
	}
	svc, _, raw, pub, _, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})

	res := svc.Handle(context.Background(), webhook.Request{Channel: "whatsapp", Token: "t", Body: []byte(`{}`)})
	if res.Outcome != webhook.OutcomeTimestampMissing {
		t.Fatalf("outcome = %s, want timestamp_missing", res.Outcome)
	}
	if len(raw.rows) != 0 || pub.calls != 0 {
		t.Fatal("no insert/publish expected on missing timestamp")
	}
}

// --- T-G4: cross-tenant idempotency segmentation.
func TestService_CrossTenantSegmentation(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{name: "whatsapp", scope: webhook.SecretScopeApp}
	svc, idem, raw, pub, tokens, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})
	body := metaPayloadAt(1_700_000_000)

	tokens.tenant = webhook.TenantID{0xaa}
	resA := svc.Handle(context.Background(), webhook.Request{Channel: "whatsapp", Token: "ta", Body: body})
	tokens.tenant = webhook.TenantID{0xbb}
	resB := svc.Handle(context.Background(), webhook.Request{Channel: "whatsapp", Token: "tb", Body: body})

	if resA.Outcome != webhook.OutcomeAccepted || resB.Outcome != webhook.OutcomeAccepted {
		t.Fatalf("expected both accepted; got %s, %s", resA.Outcome, resB.Outcome)
	}
	if len(idem.seen) != 2 {
		t.Fatalf("idempotency rows = %d, want 2 (different tenants)", len(idem.seen))
	}
	if len(raw.rows) != 2 || pub.calls != 2 {
		t.Fatalf("raw=%d publish=%d, want 2 each", len(raw.rows), pub.calls)
	}
}

// --- T-G7: timestamp in ms format → 200 + drop.
func TestService_TimestampMsRejected(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{
		name:  "whatsapp",
		scope: webhook.SecretScopeApp,
		extract: func(_ map[string][]string, _ []byte) (time.Time, error) {
			return time.Time{}, webhook.ErrTimestampFormat
		},
	}
	svc, _, raw, pub, _, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})

	res := svc.Handle(context.Background(), webhook.Request{Channel: "whatsapp", Token: "t", Body: []byte(`{}`)})
	if res.Outcome != webhook.OutcomeTimestampFormatError {
		t.Fatalf("outcome = %s, want timestamp_format_error", res.Outcome)
	}
	if len(raw.rows) != 0 || pub.calls != 0 {
		t.Fatal("no insert/publish on bad timestamp format")
	}
}

// Unknown channel → 200 + drop, no token lookup.
func TestService_UnknownChannel(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{name: "whatsapp", scope: webhook.SecretScopeApp}
	svc, _, raw, pub, tokens, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})

	res := svc.Handle(context.Background(), webhook.Request{Channel: "telegram", Token: "t", Body: []byte(`{}`)})
	if res.Outcome != webhook.OutcomeUnknownChannel {
		t.Fatalf("outcome = %s, want unknown_channel", res.Outcome)
	}
	if tokens.calls != 0 {
		t.Fatalf("TokenStore.Lookup called for unknown channel: %d", tokens.calls)
	}
	if len(raw.rows) != 0 || pub.calls != 0 {
		t.Fatal("no insert/publish expected")
	}
}

// Idempotency-key composition is sha256(tenant||':'||channel||':'||body).
// We pin it to the documented formula so a refactor that swaps order or
// delimiter fails loudly.
func TestComputeIdempotencyKey_StableAcrossArguments(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{name: "whatsapp", scope: webhook.SecretScopeApp}
	svc, _, raw, _, _, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})

	body := metaPayloadAt(1_700_000_000)
	res := svc.Handle(context.Background(), webhook.Request{Channel: "whatsapp", Token: "t", Body: body})
	if res.Outcome != webhook.OutcomeAccepted {
		t.Fatalf("outcome = %s, want accepted", res.Outcome)
	}
	if len(raw.rows) != 1 {
		t.Fatalf("raw rows = %d, want 1", len(raw.rows))
	}
	want := sha256.Sum256(append(append(append(append(res.TenantID[:],
		':'), []byte("whatsapp")...), ':'), body...))
	if !bytesEqual(raw.rows[0].IdempotencyKey, want[:]) {
		t.Fatalf("idempotency key mismatch")
	}
}

// --- T-G9 (rev 3 / F-12): cross-tenant body misrouting. Authentic Meta
//
//	payload addressed to tenant A's phone_number_id POSTed at tenant
//	B's URL → 200 + drop, outcome `tenant_body_mismatch`, NO row in
//	idempotency for B, NO publish. The cross-check fires after HMAC
//	so signature verification is unchanged.
func TestService_TenantBodyMisrouting(t *testing.T) {
	t.Parallel()
	tenantA := webhook.TenantID{0xaa}
	tenantB := webhook.TenantID{0xbb}
	adapter := &fakeAdapter{
		name:      "whatsapp",
		scope:     webhook.SecretScopeApp,
		bodyAssoc: func([]byte) (string, bool) { return "phone_for_A", true },
	}
	svc, idem, raw, pub, tokens, metrics, logger := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter}, func(c *webhook.Config) {
		c.TenantAssociationStore = &fakeAssoc{
			allow: func(t webhook.TenantID, _ string, assoc string) bool {
				return t == tenantA && assoc == "phone_for_A"
			},
		}
	})
	tokens.tenant = tenantB

	res := svc.Handle(context.Background(), webhook.Request{
		Channel: "whatsapp",
		Token:   "tB",
		Body:    metaPayloadAt(1_700_000_000),
	})
	if res.Outcome != webhook.OutcomeTenantBodyMismatch {
		t.Fatalf("outcome = %s, want tenant_body_mismatch", res.Outcome)
	}
	if res.TenantID != tenantB {
		t.Fatalf("metric tenant should be URL-resolved tenant B, got %v", res.TenantID)
	}
	// Log invariant (SecurityEngineer quality note 3): the captured log
	// record for tenant_body_mismatch MUST carry the URL-resolved tenant
	// (B), never the body's claim tenant (A — which is attacker-supplied
	// in the cross-tenant misrouting vector).
	if got := len(logger.records); got != 1 {
		t.Fatalf("captured logs = %d, want 1", got)
	}
	rec := logger.records[0]
	if rec.Outcome != webhook.OutcomeTenantBodyMismatch {
		t.Fatalf("log outcome = %s, want tenant_body_mismatch", rec.Outcome)
	}
	if !rec.HasTenantID || rec.TenantID != tenantB {
		t.Fatalf("log tenant = %v hasTenant=%v, want tenantB=%v hasTenant=true (URL-resolved, not body claim)",
			rec.TenantID, rec.HasTenantID, tenantB)
	}
	if rec.TenantID == tenantA {
		t.Fatal("log MUST NOT carry attacker-controlled body claim tenant")
	}
	if len(idem.seen) != 0 {
		t.Fatalf("idempotency rows = %d, want 0 (no insert on mismatch)", len(idem.seen))
	}
	if len(raw.rows) != 0 {
		t.Fatalf("raw rows = %d, want 0 (no insert on mismatch)", len(raw.rows))
	}
	if pub.calls != 0 {
		t.Fatalf("publish calls = %d, want 0 (no publish on mismatch)", pub.calls)
	}
	// outcome is post-HMAC authenticated for labelling (ADR §5).
	if !webhook.OutcomeTenantBodyMismatch.IsAuthenticated() {
		t.Fatal("tenant_body_mismatch must label as authenticated")
	}
	hasAuth := false
	for _, has := range metrics.receivedHasTenant {
		if has {
			hasAuth = true
		}
	}
	if !hasAuth {
		t.Fatal("metric for tenant_body_mismatch must be labelled with tenant_id")
	}
}

// Adapters with no body association (e.g., Meta page subscription
// notifications) skip the cross-check. The handler still proceeds.
func TestService_AssociationSkippedWhenAdapterReturnsFalse(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{name: "whatsapp", scope: webhook.SecretScopeApp}
	svc, idem, raw, pub, _, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter}, func(c *webhook.Config) {
		// Strict deny — but adapter returns ok=false, so it never gets called.
		c.TenantAssociationStore = &fakeAssoc{allow: func(webhook.TenantID, string, string) bool { return false }}
	})
	res := svc.Handle(context.Background(), webhook.Request{
		Channel: "whatsapp",
		Token:   "t",
		Body:    metaPayloadAt(1_700_000_000),
	})
	if res.Outcome != webhook.OutcomeAccepted {
		t.Fatalf("outcome = %s, want accepted (adapter said skip)", res.Outcome)
	}
	if len(idem.seen) != 1 || len(raw.rows) != 1 || pub.calls != 1 {
		t.Fatal("expected one full pipeline run when association check is skipped")
	}
}

// AssociationStore returning an error → internal_error, no insert.
func TestService_AssociationStoreError(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{
		name:      "whatsapp",
		scope:     webhook.SecretScopeApp,
		bodyAssoc: func([]byte) (string, bool) { return "x", true },
	}
	svc, _, raw, pub, _, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter}, func(c *webhook.Config) {
		c.TenantAssociationStore = &fakeAssoc{err: errors.New("db boom")}
	})
	res := svc.Handle(context.Background(), webhook.Request{
		Channel: "whatsapp",
		Token:   "t",
		Body:    metaPayloadAt(1_700_000_000),
	})
	if res.Outcome != webhook.OutcomeInternalError {
		t.Fatalf("outcome = %s, want internal_error", res.Outcome)
	}
	if len(raw.rows) != 0 || pub.calls != 0 {
		t.Fatal("no insert/publish on association store error")
	}
}

// Required deps that don't otherwise show up keep the linter happy.
var _ = errors.Is

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
