package webhook_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/webhook"
)

func TestSecretScope_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    webhook.SecretScope
		want string
	}{
		{webhook.SecretScopeApp, "app"},
		{webhook.SecretScopeTenant, "tenant"},
		{webhook.SecretScopeUnknown, "unknown"},
		{webhook.SecretScope(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Fatalf("(%d).String() = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestTenantID_IsZeroAndString(t *testing.T) {
	t.Parallel()
	var zero webhook.TenantID
	if !zero.IsZero() {
		t.Fatal("zero TenantID must report IsZero")
	}
	parsed, err := webhook.ParseTenantID("00000000-0000-0000-0000-0000000000aa")
	if err != nil {
		t.Fatalf("ParseTenantID: %v", err)
	}
	if parsed.IsZero() {
		t.Fatal("non-zero parse should not be zero")
	}
	if got, want := parsed.String(), "00000000-0000-0000-0000-0000000000aa"; got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
}

func TestParseTenantID_Errors(t *testing.T) {
	t.Parallel()
	cases := []string{
		"not-a-uuid",
		"00000000-0000-0000-0000-0000000000zz",
		"",
	}
	for _, c := range cases {
		c := c
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			if _, err := webhook.ParseTenantID(c); err == nil {
				t.Fatalf("ParseTenantID(%q) returned nil, want error", c)
			}
		})
	}
}

func TestParseTenantID_UppercaseHex(t *testing.T) {
	t.Parallel()
	if _, err := webhook.ParseTenantID("FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF"); err != nil {
		t.Fatalf("ParseTenantID uppercase: %v", err)
	}
}

func TestSystemClock_Now(t *testing.T) {
	t.Parallel()
	a := webhook.SystemClock{}.Now()
	if a.Location() != time.UTC {
		t.Fatalf("SystemClock.Now should be UTC, got %v", a.Location())
	}
}

func TestService_HasAdapter(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{name: "whatsapp", scope: webhook.SecretScopeApp}
	svc, _, _, _, _, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})
	if !svc.HasAdapter("whatsapp") {
		t.Fatal("registered adapter not found")
	}
	if svc.HasAdapter("telegram") {
		t.Fatal("unregistered adapter must not be found")
	}
}

func TestNewService_RequiredFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mut  func(*webhook.Config)
		want string
	}{
		{"no token store", func(c *webhook.Config) { c.TokenStore = nil }, "TokenStore"},
		{"no idem", func(c *webhook.Config) { c.IdempotencyStore = nil }, "IdempotencyStore"},
		{"no raw event", func(c *webhook.Config) { c.RawEventStore = nil }, "RawEventStore"},
		{"no publisher", func(c *webhook.Config) { c.Publisher = nil }, "Publisher"},
		{"no association store", func(c *webhook.Config) { c.TenantAssociationStore = nil }, "TenantAssociationStore"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := webhook.Config{
				TokenStore:             &fakeTokenStore{},
				IdempotencyStore:       newFakeIdem(),
				RawEventStore:          &fakeRawEventStore{},
				Publisher:              &fakePublisher{},
				TenantAssociationStore: &fakeAssoc{},
			}
			tc.mut(&cfg)
			_, err := webhook.NewService(cfg)
			if err == nil {
				t.Fatalf("NewService(%s) returned nil error", tc.name)
			}
			if !contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestNewService_DuplicateAdapter(t *testing.T) {
	t.Parallel()
	a := &fakeAdapter{name: "whatsapp", scope: webhook.SecretScopeApp}
	b := &fakeAdapter{name: "whatsapp", scope: webhook.SecretScopeApp}
	_, err := webhook.NewService(webhook.Config{
		Adapters:               []webhook.ChannelAdapter{a, b},
		TokenStore:             &fakeTokenStore{},
		IdempotencyStore:       newFakeIdem(),
		RawEventStore:          &fakeRawEventStore{},
		Publisher:              &fakePublisher{},
		TenantAssociationStore: &fakeAssoc{},
	})
	if err == nil {
		t.Fatal("duplicate registration should fail")
	}
	if !contains(err.Error(), "twice") {
		t.Fatalf("err = %v, want contains 'twice'", err)
	}
}

func TestNewService_NilAdapter(t *testing.T) {
	t.Parallel()
	_, err := webhook.NewService(webhook.Config{
		Adapters:               []webhook.ChannelAdapter{nil},
		TokenStore:             &fakeTokenStore{},
		IdempotencyStore:       newFakeIdem(),
		RawEventStore:          &fakeRawEventStore{},
		Publisher:              &fakePublisher{},
		TenantAssociationStore: &fakeAssoc{},
	})
	if err == nil {
		t.Fatal("nil adapter should fail")
	}
}

func TestService_Handle_TenantLevel_VerifyFail(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{
		name:      "psp",
		scope:     webhook.SecretScopeTenant,
		verifyTen: func(context.Context, webhook.TenantID, []byte, map[string][]string) error { return webhook.ErrSignatureInvalid },
	}
	svc, _, raw, pub, _, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})
	res := svc.Handle(context.Background(), webhook.Request{Channel: "psp", Token: "t", Body: []byte(`{}`)})
	if res.Outcome != webhook.OutcomeSignatureInvalid {
		t.Fatalf("outcome = %s, want signature_invalid", res.Outcome)
	}
	if len(raw.rows) != 0 || pub.calls != 0 {
		t.Fatal("no insert/publish on signature failure")
	}
}

func TestService_Handle_FutureTimestampOutOfWindow(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{
		name:    "whatsapp",
		scope:   webhook.SecretScopeApp,
		extract: func(map[string][]string, []byte) (time.Time, error) { return time.Unix(1_700_000_000+10*60, 0).UTC(), nil },
	}
	svc, _, raw, pub, _, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})
	res := svc.Handle(context.Background(), webhook.Request{Channel: "whatsapp", Token: "t", Body: []byte(`{}`)})
	if res.Outcome != webhook.OutcomeReplayWindowViolation {
		t.Fatalf("outcome = %s, want replay_window_violation", res.Outcome)
	}
	if len(raw.rows) != 0 || pub.calls != 0 {
		t.Fatal("no insert/publish on future-skew violation")
	}
}

func TestService_Handle_ParseError(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{
		name:  "whatsapp",
		scope: webhook.SecretScopeApp,
		parse: func([]byte) (webhook.Event, error) { return webhook.Event{}, webhook.ErrParse },
	}
	svc, _, raw, pub, _, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})
	res := svc.Handle(context.Background(), webhook.Request{Channel: "whatsapp", Token: "t", Body: []byte(`{}`)})
	if res.Outcome != webhook.OutcomeParseError {
		t.Fatalf("outcome = %s, want parse_error", res.Outcome)
	}
	// Idem already inserted before ParseEvent — the dedup row exists,
	// the raw_event does not.
	if len(raw.rows) != 0 || pub.calls != 0 {
		t.Fatal("no raw_event/publish on parse error")
	}
}

func TestService_Handle_RawEventInsertError(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{name: "whatsapp", scope: webhook.SecretScopeApp}
	svc, _, raw, pub, _, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})
	raw.insertErr = errors.New("boom")
	res := svc.Handle(context.Background(), webhook.Request{Channel: "whatsapp", Token: "t", Body: []byte(`{}`)})
	if res.Outcome != webhook.OutcomeInternalError {
		t.Fatalf("outcome = %s, want internal_error", res.Outcome)
	}
	if pub.calls != 0 {
		t.Fatal("no publish on insert error")
	}
}

func TestService_Handle_IdemError(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{name: "whatsapp", scope: webhook.SecretScopeApp}
	svc, idem, _, _, _, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})
	idem.err = errors.New("boom")
	res := svc.Handle(context.Background(), webhook.Request{Channel: "whatsapp", Token: "t", Body: []byte(`{}`)})
	if res.Outcome != webhook.OutcomeInternalError {
		t.Fatalf("outcome = %s, want internal_error", res.Outcome)
	}
}

func TestService_Handle_TokenStoreInternalError(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{name: "whatsapp", scope: webhook.SecretScopeApp}
	svc, _, _, _, tokens, _, _ := newServiceUnderTest(t, []webhook.ChannelAdapter{adapter})
	tokens.err = errors.New("connection lost")
	res := svc.Handle(context.Background(), webhook.Request{Channel: "whatsapp", Token: "t", Body: []byte(`{}`)})
	if res.Outcome != webhook.OutcomeInternalError {
		t.Fatalf("outcome = %s, want internal_error", res.Outcome)
	}
}
