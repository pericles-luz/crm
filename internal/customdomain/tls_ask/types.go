// Package tls_ask is the deny-by-default decision use-case for the Caddy
// on_demand_tls "ask" handler (SIN-62243 F45). The Caddy server consults this
// handler before issuing a Let's Encrypt certificate for an arbitrary host;
// returning anything other than Allow blocks the issuance.
//
// The use-case is hexagonal: it depends on three small ports (Repository,
// RateLimiter, FeatureFlag) and one observability port (Logger). Adapters
// in internal/adapter/* wire it to Postgres, Redis, env-flag, and slog.
//
// Design rules (deny-by-default):
//   - Any error from a port short-circuits to Deny / DenyError so that an
//     internal failure NEVER causes Caddy to issue a certificate.
//   - Empty / malformed host inputs are denied at the boundary before any
//     port is touched (no DB / Redis traffic for nuisance traffic).
//   - The rate limiter is consulted FIRST so that a flooding attacker
//     cannot drive lookup load on the database.
package tls_ask

// Decision is the outcome of an Ask call. The handler maps it to an HTTP
// status; the use-case stays transport-agnostic.
type Decision int

const (
	// DecisionUnknown is the zero value and indicates a bug in the
	// caller — every code path through Ask must return one of the
	// concrete decisions below. The handler treats it as a 5xx, never
	// as an allow.
	DecisionUnknown Decision = iota
	// DecisionAllow means the host is registered, verified, not paused,
	// and not soft-deleted. Caddy may proceed with issuance.
	DecisionAllow
	// DecisionDeny means the host fails one of the deny-by-default
	// conditions (unknown host, unverified, paused, soft-deleted).
	DecisionDeny
	// DecisionRateLimited means the rate limiter exceeded the per-host
	// budget (3 lookups / minute / host by default). The handler maps
	// this to HTTP 429.
	DecisionRateLimited
	// DecisionDisabled means the global customdomain.ask_enabled flag
	// is OFF. The handler maps this to HTTP 503 with a clear message.
	DecisionDisabled
	// DecisionError means a port returned an error. The handler maps
	// this to HTTP 5xx so Caddy retries on its own back-off; existing
	// issuances continue.
	DecisionError
)

// String returns a stable identifier suitable for structured logs.
func (d Decision) String() string {
	switch d {
	case DecisionAllow:
		return "allow"
	case DecisionDeny:
		return "deny"
	case DecisionRateLimited:
		return "rate_limited"
	case DecisionDisabled:
		return "disabled"
	case DecisionError:
		return "error"
	default:
		return "unknown"
	}
}

// Reason carries the structured reason for a Deny / Error decision so the
// handler can emit `customdomain.tls_ask_denied{host, reason}` per the
// acceptance criteria. Allowed and rate-limited decisions use the empty
// Reason.
type Reason int

const (
	// ReasonNone is the zero value used by Allow / RateLimited.
	ReasonNone Reason = iota
	// ReasonNotFound — host has no row in tenant_custom_domains, or
	// only soft-deleted rows.
	ReasonNotFound
	// ReasonNotVerified — row exists but verified_at IS NULL.
	ReasonNotVerified
	// ReasonPaused — row is non-null tls_paused_at.
	ReasonPaused
	// ReasonInvalidHost — the supplied host is empty or malformed.
	ReasonInvalidHost
	// ReasonRepositoryError — the persistence port returned an error.
	ReasonRepositoryError
	// ReasonRateLimitError — the rate-limiter port returned an error.
	ReasonRateLimitError
	// ReasonFeatureFlagError — the feature-flag port returned an error.
	ReasonFeatureFlagError
	// ReasonDisabled — the customdomain.ask_enabled flag is OFF.
	ReasonDisabled
)

// String returns the structured-log reason key. Stable across releases:
// dashboards depend on it.
func (r Reason) String() string {
	switch r {
	case ReasonNotFound:
		return "not_found"
	case ReasonNotVerified:
		return "not_verified"
	case ReasonPaused:
		return "paused"
	case ReasonInvalidHost:
		return "invalid_host"
	case ReasonRepositoryError:
		return "repository_error"
	case ReasonRateLimitError:
		return "rate_limit_error"
	case ReasonFeatureFlagError:
		return "feature_flag_error"
	case ReasonDisabled:
		return "disabled"
	default:
		return ""
	}
}

// Result is what Ask returns. Decision drives the HTTP status; Reason and
// Host drive the structured log; Err carries the underlying port error
// (only set when Decision == DecisionError).
type Result struct {
	Decision Decision
	Reason   Reason
	Host     string
	Err      error
}
