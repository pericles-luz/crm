package webhook

import (
	"context"
	"errors"
	"time"
)

// ChannelAdapter encapsulates the per-channel concerns: HMAC verify
// (app- or tenant-scoped), timestamp extraction, and payload parsing.
// Adapters live in internal/adapter/channel/<provider>/ and never bleed
// SDK types into the domain.
//
// The two Verify methods are mutually exclusive: SecretScopeApp adapters
// implement VerifyApp and return ErrUnsupportedScope from VerifyTenant;
// SecretScopeTenant adapters do the inverse. The Service routes calls
// based on SecretScope() at adapter-registration time.
type ChannelAdapter interface {
	// Name returns the channel identifier; MUST match [a-z0-9_]+.
	Name() string

	// SecretScope declares whether the HMAC secret is app- or
	// tenant-scoped. SecretScopeUnknown causes startup to fail-fast.
	SecretScope() SecretScope

	// VerifyApp checks the HMAC signature against the app-level secret.
	// Called only when SecretScope() == SecretScopeApp.
	VerifyApp(ctx context.Context, body []byte, headers map[string][]string) error

	// VerifyTenant checks the HMAC signature against a tenant-scoped
	// secret. Called only when SecretScope() == SecretScopeTenant. The
	// ctx provided here carries the *claim* tenantID — the implementation
	// must NOT log/metricize it before the verify returns nil.
	VerifyTenant(ctx context.Context, tenantID TenantID, body []byte, headers map[string][]string) error

	// ExtractTimestamp returns the source-of-truth timestamp from the
	// payload. ADR §2 D3 forbids HTTP `Date` fallbacks. Returns
	// ErrTimestampMissing when the payload field is absent and
	// ErrTimestampFormat when present but malformed (e.g. ms instead of
	// seconds).
	ExtractTimestamp(headers map[string][]string, body []byte) (time.Time, error)

	// ParseEvent extracts a channel-agnostic Event from body. Returns
	// ErrParse when malformed; the service treats parse errors as
	// silently dropped (200 OK) per anti-enumeration.
	ParseEvent(body []byte) (Event, error)

	// BodyTenantAssociation extracts the provider-specific identifier
	// that declares which tenant the body is addressed to (rev 3 / F-12).
	// For Meta Cloud this is `entry[0].changes[0].value.metadata.
	// phone_number_id`. Returns (assoc, true) when the payload exposes
	// such an identifier; (_, false) when the body has no usable
	// association field for that channel — in which case the service
	// skips the cross-tenant check (documented per-adapter).
	BodyTenantAssociation(body []byte) (string, bool)
}

// TokenStore looks up a webhook token by (channel, sha256(token)) and
// returns the resolved tenant id. Lookups MUST use the partial unique
// index `(channel, token_hash) WHERE revoked_at IS NULL` — see ADR §3.
//
// Returns ErrTokenUnknown for misses (no row), ErrTokenRevoked when the
// hash matches but revoked_at is set within the overlap window. Both
// outcomes map to a silent 200 at the handler.
type TokenStore interface {
	// Lookup resolves a token hash to its tenant.
	Lookup(ctx context.Context, channel string, tokenHash []byte, now time.Time) (TenantID, error)
	// MarkUsed bumps last_used_at for monitoring; non-fatal on error
	// (best-effort).
	MarkUsed(ctx context.Context, channel string, tokenHash []byte, now time.Time) error
}

// IdempotencyStore records a (tenant, channel, sha256(payload)) tuple
// exactly once. Implementations MUST use INSERT … ON CONFLICT DO NOTHING
// against the primary key declared in 0075b — see ADR §2 D2.
//
// CheckAndStore returns (firstSeen=true) when the row was newly inserted,
// (firstSeen=false) when a conflicting row already exists (= replay).
type IdempotencyStore interface {
	CheckAndStore(ctx context.Context, tenantID TenantID, channel string, key []byte, now time.Time) (firstSeen bool, err error)
}

// TenantAssociationStore answers the cross-check "does this body
// association belong to this tenant on this channel?" (rev 3 / F-12).
// The PK in 0075a2 (channel, association) guarantees one association
// maps to one tenant, so a scalar boolean is enough.
type TenantAssociationStore interface {
	// CheckAssociation returns (true, nil) when the (tenant_id, channel,
	// association) tuple exists. Returns (false, nil) when the
	// (channel, association) row exists but is owned by a different
	// tenant OR does not exist at all — both are misroutings from the
	// caller's POV. Implementations MUST NOT distinguish those two
	// failure modes to the caller, to avoid leaking association presence.
	CheckAssociation(ctx context.Context, tenantID TenantID, channel, association string) (bool, error)
}

// RawEventStore appends an event row to raw_event for forensics and
// outbox-lite reconciliation (ADR §2 D7). published_at remains NULL until
// EventPublisher succeeds, at which point MarkPublished is called.
type RawEventStore interface {
	Insert(ctx context.Context, row RawEventRow) (eventID [16]byte, err error)
	MarkPublished(ctx context.Context, eventID [16]byte, now time.Time) error
}

// RawEventRow is the storage shape for raw_event. Headers are passed as
// a JSON-serializable map so the adapter doesn't leak HTTP types.
type RawEventRow struct {
	TenantID       TenantID
	Channel        string
	IdempotencyKey []byte
	Payload        []byte
	Headers        map[string][]string
	ReceivedAt     time.Time
}

// EventPublisher fan-outs the accepted event to the downstream broker
// (NATS in production). The service treats publish failures as
// non-blocking: the row is left published_at=NULL and the reconciler
// (D7) retries.
type EventPublisher interface {
	Publish(ctx context.Context, eventID [16]byte, tenantID TenantID, channel string, payload []byte, headers map[string][]string) error
}

// Clock decouples the service from `time.Now` for deterministic tests.
// SystemClock is the production implementation; tests use a Fake that
// returns a fixed instant.
type Clock interface {
	Now() time.Time
}

// SystemClock returns time.Now() in UTC.
type SystemClock struct{}

// Now implements Clock.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// Logger is the minimal structured-logging port. The service emits a
// single record per request with the outcome and a small set of safe
// fields; tenant_id is only present when the outcome is authenticated.
//
// The lint custom mentioned in ADR §5 (paperclip-lint nosecrets)
// rejects `webhook_token`, `raw_payload`, and pre-HMAC `tenant_id` from
// any call site; this port intentionally accepts a small typed struct
// rather than free-form fields so the lint surface stays narrow.
type Logger interface {
	LogResult(ctx context.Context, rec LogRecord)
}

// LogRecord is the strict shape of a webhook log entry.
type LogRecord struct {
	RequestID   string
	Channel     string
	Outcome     Outcome
	ReceivedAt  time.Time
	TenantID    TenantID // zero value pre-HMAC; populated post-auth only
	HasTenantID bool
	Err         error
}

// Metrics is the minimal observability port. Counters mirror §5 of the
// ADR. The implementation MUST NOT emit tenant_id labels for outcomes
// where Outcome.IsAuthenticated() is false.
type Metrics interface {
	IncReceived(channel string, outcome Outcome, tenantID TenantID, hasTenant bool)
	ObserveAck(channel string, d time.Duration)
	IncIdempotencyConflict(channel string, tenantID TenantID)
}

// Sentinel errors. Adapters and stores return these so the Service can
// translate to outcomes uniformly.
var (
	ErrTokenUnknown        = errors.New("webhook: token unknown")
	ErrTokenRevoked        = errors.New("webhook: token revoked")
	ErrSignatureInvalid    = errors.New("webhook: signature invalid")
	ErrTimestampMissing    = errors.New("webhook: timestamp missing")
	ErrTimestampFormat     = errors.New("webhook: timestamp format error")
	ErrTimestampOutOfRange = errors.New("webhook: timestamp out of range")
	ErrParse               = errors.New("webhook: parse error")
	ErrUnsupportedScope    = errors.New("webhook: adapter does not support this verify path")
)
