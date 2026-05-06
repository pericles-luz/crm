// Package webhook is the domain core for the webhook intake pipeline
// (SIN-62234 / ADR 0075). The domain layer contains pure types and the
// orchestrating service: it never imports database/sql, net/http, or any
// channel SDK — those live behind ports declared in ports.go.
package webhook

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// SecretScope declares whether a ChannelAdapter's HMAC secret is
// app-wide (one secret per channel/app) or tenant-scoped (one secret per
// tenant). Adapters with mixed/unknown scopes are rejected at startup —
// see Service.RegisterAdapter.
type SecretScope int

const (
	// SecretScopeUnknown is the zero value; no adapter may declare it.
	// Used as a guard so accidental zero-valued adapters fail-fast.
	SecretScopeUnknown SecretScope = iota
	// SecretScopeApp — single app-level secret. HMAC verifies *before*
	// resolving the tenant. Example: Meta Cloud (app_secret in env).
	SecretScopeApp
	// SecretScopeTenant — secret is tenant-scoped. Tenant is resolved
	// *first* via webhook_token; HMAC verifies *after* loading the secret.
	// Example: hypothetical PSP adapters.
	SecretScopeTenant
)

// String returns the canonical lowercase name used in metrics labels and
// fail-fast diagnostics.
func (s SecretScope) String() string {
	switch s {
	case SecretScopeApp:
		return "app"
	case SecretScopeTenant:
		return "tenant"
	default:
		return "unknown"
	}
}

// Outcome enumerates the terminal classification of a webhook request.
// Names are stable: they appear as Prometheus label values (§5 of ADR 0075).
//
// Pre-HMAC outcomes (no authenticated tenant) MUST NOT be labelled with
// tenant_id; the wrapper AuthenticatedTenantID enforces this invariant
// for callers that respect the contract.
type Outcome string

const (
	OutcomeAccepted              Outcome = "accepted"
	OutcomeReplay                Outcome = "replay"
	OutcomeUnknownToken          Outcome = "unknown_token"
	OutcomeRevokedToken          Outcome = "revoked_token"
	OutcomeSignatureInvalid      Outcome = "signature_invalid"
	OutcomeReplayWindowViolation Outcome = "replay_window_violation"
	OutcomeTimestampMissing      Outcome = "timestamp_missing"
	OutcomeTimestampFormatError  Outcome = "timestamp_format_error"
	OutcomeParseError            Outcome = "parse_error"
	OutcomeUnknownChannel        Outcome = "unknown_channel"
	OutcomeInternalError         Outcome = "internal_error"
	// OutcomeTenantBodyMismatch — rev 3 / F-12. The HMAC was valid and a
	// tenant was resolved by URL token, but the body declares an
	// association (e.g. Meta phone_number_id) that does not belong to
	// that tenant in tenant_channel_associations. Treated as authenticated
	// for metric labelling purposes per ADR §5.
	OutcomeTenantBodyMismatch Outcome = "tenant_body_mismatch"
)

// IsAuthenticated returns true if the outcome is reached only after the
// tenant identity is HMAC-authenticated. Used by the metrics adapter to
// decide whether tenant_id labels are safe to emit. Per ADR §5,
// `tenant_body_mismatch` is post-HMAC and labels with the URL-resolved
// tenant id (the legitimate destination, even though the body itself is
// invalid for it) — so it counts as authenticated for labelling.
func (o Outcome) IsAuthenticated() bool {
	switch o {
	case OutcomeAccepted, OutcomeReplay, OutcomeReplayWindowViolation,
		OutcomeTenantBodyMismatch:
		return true
	default:
		return false
	}
}

// Event is the parsed, channel-agnostic representation of an inbound
// webhook payload. Adapters produce this via ChannelAdapter.ParseEvent;
// the service treats it as opaque metadata for the publish step.
type Event struct {
	// Timestamp is the source-of-truth time as declared by the channel
	// payload (e.g. Meta `entry[].time`). HTTP `Date` MUST NOT be used —
	// see ADR §2 D3.
	Timestamp time.Time
	// Channel echoed back from the adapter; equals the registered Name().
	Channel string
	// External identifier from the payload (best-effort, debug only).
	ExternalID string
}

// channelNameRegex enforces ADR §2 D2: channel ∈ [a-z0-9_]+. The `:`
// character is intentionally excluded so the idempotency-key composition
// (`tenant_id || ':' || channel || ':' || payload`) parses unambiguously.
var channelNameRegex = regexp.MustCompile(`^[a-z0-9_]+$`)

// ValidateChannelName returns nil iff name matches [a-z0-9_]+. Used by
// Service.RegisterAdapter at startup.
func ValidateChannelName(name string) error {
	if name == "" {
		return errors.New("channel name is empty")
	}
	if !channelNameRegex.MatchString(name) {
		return fmt.Errorf("channel name %q must match [a-z0-9_]+", name)
	}
	return nil
}

// ValidateSecretScope returns nil iff scope is one of the documented
// values. Adapters returning SecretScopeUnknown (the zero value) fail-fast.
func ValidateSecretScope(scope SecretScope) error {
	switch scope {
	case SecretScopeApp, SecretScopeTenant:
		return nil
	default:
		return fmt.Errorf("invalid SecretScope %d (must be App or Tenant)", scope)
	}
}

// authenticatedTenantKey is an unexported context key — declared as a
// distinct type to avoid collision with other packages. The only writer
// is Service after a successful HMAC verification.
type authenticatedTenantKey struct{}

// withAuthenticatedTenant attaches a verified tenant id to ctx. Internal
// to the webhook package; adapters never write this directly.
func withAuthenticatedTenant(ctx context.Context, tenantID TenantID) context.Context {
	return context.WithValue(ctx, authenticatedTenantKey{}, tenantID)
}

// AuthenticatedTenantID retrieves the tenant id from ctx iff Service has
// previously HMAC-authenticated the request. Returns (zero, false)
// otherwise. ADR §2 D4 invariant: any pre-HMAC code path that mistakes
// the claim tenant_id for an authenticated identity is contractually
// wrong; this is the only sanctioned getter.
func AuthenticatedTenantID(ctx context.Context) (TenantID, bool) {
	v, ok := ctx.Value(authenticatedTenantKey{}).(TenantID)
	return v, ok
}

// TenantID is a 16-byte UUID, kept as a fixed-width array so the
// idempotency-key composition is unambiguous (ADR §2 D2). Callers
// converting from external UUID libraries should validate length.
type TenantID [16]byte

// IsZero reports whether t is the zero value.
func (t TenantID) IsZero() bool { return t == TenantID{} }

// String renders the canonical 8-4-4-4-12 hex form.
func (t TenantID) String() string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		t[0:4], t[4:6], t[6:8], t[8:10], t[10:16])
}

// ParseTenantID parses canonical UUID strings (with or without dashes)
// into a TenantID. Used by adapters at the boundary; the domain itself
// only sees TenantID values.
func ParseTenantID(s string) (TenantID, error) {
	var t TenantID
	hex := make([]byte, 0, 32)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '-' {
			continue
		}
		hex = append(hex, c)
	}
	if len(hex) != 32 {
		return t, fmt.Errorf("invalid uuid %q: want 32 hex chars, got %d", s, len(hex))
	}
	for i := 0; i < 16; i++ {
		hi, err := hexNibble(hex[2*i])
		if err != nil {
			return t, fmt.Errorf("invalid uuid %q: %w", s, err)
		}
		lo, err := hexNibble(hex[2*i+1])
		if err != nil {
			return t, fmt.Errorf("invalid uuid %q: %w", s, err)
		}
		t[i] = hi<<4 | lo
	}
	return t, nil
}

func hexNibble(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	default:
		return 0, fmt.Errorf("non-hex char %q", c)
	}
}
