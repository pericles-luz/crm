// Package meta implements the Meta Cloud webhook adapter
// (WhatsApp / Instagram / Facebook). SecretScope is AppLevel: a single
// app_secret signs all events for the configured Meta app.
//
// HMAC: SHA-256 over the raw request body, key = app_secret. Header is
// `X-Hub-Signature-256: sha256=<hex>` (case-insensitive). ADR 0075 §2 D3
// requires `entry[].time` (Unix seconds, 10 digits) as the timestamp;
// HTTP `Date` fallback is forbidden.
package meta

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pericles-luz/crm/internal/webhook"
)

// Adapter is the Meta Cloud ChannelAdapter. The channel name is fixed
// per Meta product family (whatsapp / instagram / facebook); a single
// app secret signs all of them but each maps to a separate registered
// adapter so metrics labels stay clean.
type Adapter struct {
	channel   string
	appSecret []byte
}

// New constructs an adapter for the given Meta channel. channel must be
// one of "whatsapp", "instagram", "facebook". appSecret is the Meta app
// secret loaded from env at startup; passing an empty secret returns an
// error so cmd/server fails-fast.
func New(channel, appSecret string) (*Adapter, error) {
	if err := webhook.ValidateChannelName(channel); err != nil {
		return nil, err
	}
	switch channel {
	case "whatsapp", "instagram", "facebook":
	default:
		return nil, fmt.Errorf("meta: unsupported channel %q (allowed: whatsapp,instagram,facebook)", channel)
	}
	if appSecret == "" {
		return nil, fmt.Errorf("meta: app secret for channel %q is empty", channel)
	}
	return &Adapter{channel: channel, appSecret: []byte(appSecret)}, nil
}

// Name implements webhook.ChannelAdapter.
func (a *Adapter) Name() string { return a.channel }

// SecretScope implements webhook.ChannelAdapter.
func (*Adapter) SecretScope() webhook.SecretScope { return webhook.SecretScopeApp }

// VerifyApp implements webhook.ChannelAdapter. Signature header is
// case-insensitive; comparison uses hmac.Equal (constant-time).
func (a *Adapter) VerifyApp(_ context.Context, body []byte, headers map[string][]string) error {
	got, ok := signatureHeader(headers)
	if !ok {
		return webhook.ErrSignatureInvalid
	}
	mac := hmac.New(sha256.New, a.appSecret)
	_, _ = mac.Write(body)
	want := mac.Sum(nil)
	gotBytes, err := hex.DecodeString(got)
	if err != nil {
		return webhook.ErrSignatureInvalid
	}
	if !hmac.Equal(gotBytes, want) {
		return webhook.ErrSignatureInvalid
	}
	return nil
}

// VerifyTenant implements webhook.ChannelAdapter; AppLevel scope returns
// ErrUnsupportedScope so a misconfigured router fails loudly in tests.
func (*Adapter) VerifyTenant(context.Context, webhook.TenantID, []byte, map[string][]string) error {
	return webhook.ErrUnsupportedScope
}

// metaPayload is a permissive subset of the Meta webhook envelope.
// We need:
//   - entry[].time            for timestamp extraction (ADR §2 D3)
//   - entry[0].changes[0].value.metadata.phone_number_id for the
//     body↔tenant cross-check (rev 3 / F-12)
//
// Everything else is left to downstream consumers via raw_event.
type metaPayload struct {
	Entry []struct {
		ID      string `json:"id"`
		Time    int64  `json:"time"`
		Changes []struct {
			Value struct {
				Metadata struct {
					PhoneNumberID string `json:"phone_number_id"`
				} `json:"metadata"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// ExtractTimestamp implements webhook.ChannelAdapter. Returns
// ErrTimestampMissing when entry is empty/absent and ErrTimestampFormat
// when the magnitude looks like milliseconds (>10^12) — see T-G7.
func (*Adapter) ExtractTimestamp(_ map[string][]string, body []byte) (time.Time, error) {
	var p metaPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return time.Time{}, fmt.Errorf("%w: %v", webhook.ErrTimestampMissing, err)
	}
	if len(p.Entry) == 0 || p.Entry[0].Time == 0 {
		return time.Time{}, webhook.ErrTimestampMissing
	}
	t := p.Entry[0].Time
	if t < 0 {
		return time.Time{}, webhook.ErrTimestampFormat
	}
	if t > 1_000_000_000_000 { // >10^12 means ms (or worse) — ADR §2 D3.
		return time.Time{}, webhook.ErrTimestampFormat
	}
	return time.Unix(t, 0).UTC(), nil
}

// ParseEvent implements webhook.ChannelAdapter. We extract a minimal
// channel-agnostic shape; downstream consumers read raw_event.payload
// when they need details.
func (a *Adapter) ParseEvent(body []byte) (webhook.Event, error) {
	var p metaPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return webhook.Event{}, fmt.Errorf("%w: %v", webhook.ErrParse, err)
	}
	if len(p.Entry) == 0 {
		return webhook.Event{}, webhook.ErrParse
	}
	first := p.Entry[0]
	return webhook.Event{
		Timestamp:  time.Unix(first.Time, 0).UTC(),
		Channel:    a.channel,
		ExternalID: first.ID,
	}, nil
}

// BodyTenantAssociation implements webhook.ChannelAdapter (rev 3 /
// F-12, fail-closed sub-rule per follow-up SecurityEngineer 62d7529c).
//
// Contract for this Meta adapter — explicit because reviewers will
// audit it and the rule is non-obvious:
//
//   - The current scope of this adapter is **tenant-scoped Meta events**
//     (WhatsApp messages, message-status callbacks, IG/FB inbound
//     messages). Every such event includes a tenant identifier under
//     `entry[0].changes[0].value.metadata.phone_number_id` per Meta's
//     documented schema. The handler MUST cross-check that identifier
//     against tenant_channel_associations before treating the body as
//     authenticated for the URL-resolved tenant.
//
//   - Therefore this method ALWAYS returns ok=true. We never return
//     ok=false — that would skip the cross-check, opening a vector
//     where an attacker submits a Meta-shape body with the phone_number_id
//     surgically removed and gets a free pass. Fail-closed by design:
//
//   - body with phone_number_id present  → (id, true).
//
//   - body parseable but field missing   → ("", true) — cross-check
//     fails with outcome `tenant_body_mismatch`.
//
//   - body completely malformed JSON     → ("", true) — same; the
//     request is dropped here rather than reaching ParseEvent. Either
//     drop is silent 200 anti-enumeration; the operator distinguishes
//     them via metric outcome labels.
//
//   - When (and only when) this adapter is extended to support Meta
//     event types that do NOT carry a phone_number_id (e.g. account-
//     level subscription changes), that branch MUST add a comment of
//     the form `// SecretScope justification:` next to the ok=false
//     return — the convention test in adapter/channel asserts that
//     marker is present whenever a `false` literal is returned from a
//     BodyTenantAssociation method. Approving such a path is a
//     SecurityEngineer review item.
func (*Adapter) BodyTenantAssociation(body []byte) (string, bool) {
	var p metaPayload
	if err := json.Unmarshal(body, &p); err != nil {
		// Fail-closed: cross-check will fail with tenant_body_mismatch.
		return "", true
	}
	if len(p.Entry) == 0 || len(p.Entry[0].Changes) == 0 {
		// Fail-closed: every supported Meta event carries
		// entry[0].changes[0]; absence implies either an unsupported
		// event type or a tampered body.
		return "", true
	}
	id := p.Entry[0].Changes[0].Value.Metadata.PhoneNumberID
	if id == "" {
		// Fail-closed: phone_number_id is mandatory in Meta's "messages"
		// schema; absence is a tampering signal, not a legitimate skip.
		return "", true
	}
	return id, true
}

// signatureHeader returns the hex digest from `X-Hub-Signature-256`,
// stripping the `sha256=` prefix when present. Header lookup is
// case-insensitive.
func signatureHeader(headers map[string][]string) (string, bool) {
	for k, v := range headers {
		if strings.EqualFold(k, "X-Hub-Signature-256") && len(v) > 0 {
			val := strings.TrimSpace(v[0])
			if strings.HasPrefix(val, "sha256=") {
				return val[len("sha256="):], true
			}
			return val, true
		}
	}
	return "", false
}
