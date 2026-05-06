package tls_ask

import (
	"context"
	"errors"
	"time"
)

// Repository hides the persistence layer behind a narrow lookup contract.
// The Postgres adapter (internal/adapter/store/postgres) is the production
// implementation; the use-case tests use an in-memory fake.
//
// The contract is intentionally MINIMAL:
//   - Lookup returns the snapshot of the active row for a given host
//     ("active" = deleted_at IS NULL). It is the use-case's job to
//     interpret VerifiedAt / TLSPausedAt; the adapter must NOT pre-filter.
//   - ErrNotFound is the sentinel for "no active row exists for this
//     host". Use errors.Is to detect.
//   - Any other error is treated as a transient repository fault and
//     denied with DecisionError + ReasonRepositoryError.
type Repository interface {
	Lookup(ctx context.Context, host string) (DomainRecord, error)
}

// DomainRecord is the use-case-facing projection of one tenant_custom_domains
// row. Only the columns the deny-by-default decision needs are exposed.
type DomainRecord struct {
	// VerifiedAt is non-nil iff the tenant proved control of the host.
	VerifiedAt *time.Time
	// TLSPausedAt is non-nil while ops have frozen issuance for this host
	// (e.g. while investigating a rebind incident).
	TLSPausedAt *time.Time
}

// ErrNotFound is the sentinel returned by Repository.Lookup when no active
// (non-soft-deleted) row exists for the requested host.
var ErrNotFound = errors.New("tls_ask: domain not found")

// RateLimiter caps the number of /internal/tls/ask lookups per host per
// minute. The contract is "atomic check-and-increment": Allow returns true
// if the call fits under the per-host budget AND records the call against
// the budget; false means "deny this call" and no other side-effect.
//
// Errors propagate (not silenced) so the use-case can decide whether a
// transient Redis fault should fail-closed (the F45 default) or fail-open
// (not used here, but possible in a non-security context).
type RateLimiter interface {
	Allow(ctx context.Context, host string, now time.Time) (bool, error)
}

// FeatureFlag exposes the customdomain.ask_enabled global kill-switch.
// Deny-by-default: a port error returns false-with-error, which the
// use-case maps to DecisionError so Caddy retries (issuances in flight
// continue independently).
type FeatureFlag interface {
	AskEnabled(ctx context.Context) (bool, error)
}

// Logger is the structured-log port the handler relays into. It accepts a
// flat key/value attribute list to match slog's semantics without forcing
// the domain layer to import slog.
type Logger interface {
	// LogDeny emits `customdomain.tls_ask_denied` at INFO with the host
	// and reason. Allow path uses LogAllow; both must be safe for high
	// fan-out (Caddy queries on every TLS handshake for unknown hosts).
	LogDeny(ctx context.Context, host string, reason Reason)
	// LogAllow emits `customdomain.tls_ask_allow` at INFO. The handler
	// keeps allow-side cardinality bounded by relying on Caddy's own
	// cert-cache (we won't see the same host more than ~once an hour
	// after the first issuance).
	LogAllow(ctx context.Context, host string)
	// LogError emits `customdomain.tls_ask_error` at ERROR with the
	// reason and the underlying error. Used for repository / rate-limit
	// / flag failures.
	LogError(ctx context.Context, host string, reason Reason, err error)
}
