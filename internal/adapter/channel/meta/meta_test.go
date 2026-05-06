package meta_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	metaadapter "github.com/pericles-luz/crm/internal/adapter/channel/meta"
	"github.com/pericles-luz/crm/internal/webhook"
)

const testSecret = "super-secret"

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestNew_ValidatesArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		channel string
		secret  string
		wantErr string
	}{
		{"empty channel", "", "s", "channel name is empty"},
		{"bad channel chars", "BadName", "s", "[a-z0-9_]+"},
		{"unsupported channel", "telegram", "s", "unsupported channel"},
		{"empty secret", "whatsapp", "", "app secret"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := metaadapter.New(c.channel, c.secret)
			if err == nil {
				t.Fatalf("New(%q,%q) returned nil error", c.channel, c.secret)
			}
		})
	}
}

func TestVerifyApp_HappyPathAndMutations(t *testing.T) {
	t.Parallel()
	a, err := metaadapter.New("whatsapp", testSecret)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := []byte(`{"entry":[{"id":"1","time":1700000000}]}`)
	sig := sign(body)
	headers := map[string][]string{"X-Hub-Signature-256": {sig}}

	if err := a.VerifyApp(context.Background(), body, headers); err != nil {
		t.Fatalf("VerifyApp: %v", err)
	}
	// Mutating any byte invalidates the signature.
	bad := append([]byte(nil), body...)
	bad[0] = '['
	if err := a.VerifyApp(context.Background(), bad, headers); !errors.Is(err, webhook.ErrSignatureInvalid) {
		t.Fatalf("VerifyApp on tampered body: %v, want ErrSignatureInvalid", err)
	}
}

func TestVerifyApp_HeaderMissingOrMalformed(t *testing.T) {
	t.Parallel()
	a, _ := metaadapter.New("whatsapp", testSecret)
	body := []byte(`{}`)

	if err := a.VerifyApp(context.Background(), body, nil); !errors.Is(err, webhook.ErrSignatureInvalid) {
		t.Fatalf("missing header: %v", err)
	}
	bad := map[string][]string{"X-Hub-Signature-256": {"sha256=zzznothex"}}
	if err := a.VerifyApp(context.Background(), body, bad); !errors.Is(err, webhook.ErrSignatureInvalid) {
		t.Fatalf("non-hex header: %v", err)
	}
}

func TestVerifyApp_AcceptsHeaderWithoutPrefix(t *testing.T) {
	t.Parallel()
	a, _ := metaadapter.New("whatsapp", testSecret)
	body := []byte(`{"x":1}`)
	mac := hmac.New(sha256.New, []byte(testSecret))
	_, _ = mac.Write(body)
	digest := hex.EncodeToString(mac.Sum(nil))
	headers := map[string][]string{"X-Hub-Signature-256": {digest}} // no sha256= prefix
	if err := a.VerifyApp(context.Background(), body, headers); err != nil {
		t.Fatalf("VerifyApp: %v", err)
	}
}

func TestVerifyTenant_AppLevelReturnsUnsupported(t *testing.T) {
	t.Parallel()
	a, _ := metaadapter.New("whatsapp", testSecret)
	if err := a.VerifyTenant(context.Background(), webhook.TenantID{}, nil, nil); !errors.Is(err, webhook.ErrUnsupportedScope) {
		t.Fatalf("VerifyTenant: %v, want ErrUnsupportedScope", err)
	}
}

func TestExtractTimestamp(t *testing.T) {
	t.Parallel()
	a, _ := metaadapter.New("whatsapp", testSecret)
	cases := []struct {
		name    string
		body    string
		wantErr error
	}{
		{"ok", `{"entry":[{"id":"1","time":1700000000}]}`, nil},
		{"missing entry", `{"entry":[]}`, webhook.ErrTimestampMissing},
		{"missing time", `{"entry":[{"id":"1"}]}`, webhook.ErrTimestampMissing},
		{"ms format", `{"entry":[{"id":"1","time":1700000000000}]}`, webhook.ErrTimestampFormat},
		{"negative", `{"entry":[{"id":"1","time":-1}]}`, webhook.ErrTimestampFormat},
		{"malformed", `{not json`, webhook.ErrTimestampMissing},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := a.ExtractTimestamp(nil, []byte(tc.body))
			if (tc.wantErr == nil) != (err == nil) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestParseEvent(t *testing.T) {
	t.Parallel()
	a, _ := metaadapter.New("whatsapp", testSecret)

	body := []byte(`{"entry":[{"id":"abc","time":1700000000}]}`)
	ev, err := a.ParseEvent(body)
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if ev.ExternalID != "abc" {
		t.Fatalf("ExternalID = %q, want %q", ev.ExternalID, "abc")
	}
	if ev.Channel != "whatsapp" {
		t.Fatalf("Channel = %q, want whatsapp", ev.Channel)
	}

	if _, err := a.ParseEvent([]byte(`{not json`)); !errors.Is(err, webhook.ErrParse) {
		t.Fatalf("ParseEvent malformed = %v, want ErrParse", err)
	}
	if _, err := a.ParseEvent([]byte(`{"entry":[]}`)); !errors.Is(err, webhook.ErrParse) {
		t.Fatalf("ParseEvent empty = %v, want ErrParse", err)
	}
}

// rev 3 / F-12 + fail-closed sub-rule (SecurityEngineer follow-up
// 62d7529c): BodyTenantAssociation extracts phone_number_id from
// `entry[0].changes[0].value.metadata.phone_number_id`. The Meta
// adapter ALWAYS returns ok=true — empty assoc on missing/malformed
// fields, which makes the body↔tenant cross-check fail-closed with
// outcome `tenant_body_mismatch`. ok=false is reserved for future Meta
// event types that legitimately carry no tenant identifier and is
// gated by the convention test (`// SecretScope justification:` marker).
func TestBodyTenantAssociation(t *testing.T) {
	t.Parallel()
	a, _ := metaadapter.New("whatsapp", testSecret)

	cases := []struct {
		name string
		body string
		want string
		ok   bool
	}{
		{
			"messages payload with phone_number_id",
			`{"entry":[{"id":"1","time":1700000000,"changes":[{"value":{"metadata":{"phone_number_id":"PA"}}}]}]}`,
			"PA", true,
		},
		// Fail-closed: every non-happy path returns ok=true with empty
		// assoc so the cross-check fires and produces tenant_body_mismatch.
		{
			"empty entry → fail-closed",
			`{"entry":[]}`,
			"", true,
		},
		{
			"entry without changes (legitimate-looking but unsupported here) → fail-closed",
			`{"entry":[{"id":"1","time":1700000000}]}`,
			"", true,
		},
		{
			"changes without metadata.phone_number_id → fail-closed",
			`{"entry":[{"id":"1","time":1700000000,"changes":[{"value":{"metadata":{}}}]}]}`,
			"", true,
		},
		{
			"malformed json → fail-closed",
			`{not json`,
			"", true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := a.BodyTenantAssociation([]byte(tc.body))
			if got != tc.want || ok != tc.ok {
				t.Fatalf("BodyTenantAssociation = (%q,%v), want (%q,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// Attacker takes a Meta-valid messages body and surgically removes the
// phone_number_id field. The adapter MUST NOT silently let this through
// (which a naive ok=false implementation would do): the cross-check
// SHOULD fire, and SHOULD fail. We assert the contract at the adapter
// layer; T-G9 in service_test.go covers the end-to-end outcome.
func TestBodyTenantAssociation_FailClosedOnSurgicalRemoval(t *testing.T) {
	t.Parallel()
	a, _ := metaadapter.New("whatsapp", testSecret)

	body := `{"entry":[{"id":"1","time":1700000000,"changes":[{"value":{"metadata":{"display_phone_number":"+5511999"}}}]}]}`
	got, ok := a.BodyTenantAssociation([]byte(body))
	if !ok {
		t.Fatal("ok=false on tampered body would skip cross-check (security regression). Want ok=true with empty assoc.")
	}
	if got != "" {
		t.Fatalf("assoc = %q, want \"\" (cross-check should fail)", got)
	}
}

func TestNameAndScope(t *testing.T) {
	t.Parallel()
	a, _ := metaadapter.New("instagram", testSecret)
	if a.Name() != "instagram" {
		t.Fatalf("Name = %q", a.Name())
	}
	if a.SecretScope() != webhook.SecretScopeApp {
		t.Fatalf("SecretScope = %v, want App", a.SecretScope())
	}
}
