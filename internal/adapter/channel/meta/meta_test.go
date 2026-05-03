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

// rev 3 / F-12: BodyTenantAssociation extracts phone_number_id from
// `entry[0].changes[0].value.metadata.phone_number_id`. Returns
// (id, true) for a typical messages payload, ("", false) for envelopes
// without the field (page subscription change, malformed bodies).
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
		{
			"empty entry",
			`{"entry":[]}`,
			"", false,
		},
		{
			"entry without changes (page subscription)",
			`{"entry":[{"id":"1","time":1700000000}]}`,
			"", false,
		},
		{
			"changes without metadata.phone_number_id",
			`{"entry":[{"id":"1","time":1700000000,"changes":[{"value":{"metadata":{}}}]}]}`,
			"", false,
		},
		{
			"malformed json",
			`{not json`,
			"", false,
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
