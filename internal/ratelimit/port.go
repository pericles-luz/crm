// Package ratelimit declares the hexagonal port through which the application
// asks "may this request happen now?".
//
// The contract follows ADR 0081 §3 (SIN-62245). Domain code (use cases,
// middleware) imports only this package; concrete adapters live under
// internal/ratelimit/adapter/* and are the only ones allowed to talk to
// Redis or any other backing store.
package ratelimit

import (
	"context"
	"errors"
	"time"
)

// Limit is the canonical "max events per window" budget for a bucket. Window
// is the rolling period; Max is the number of events permitted within the
// window. A zero-value Limit is treated as "no limit configured" by callers
// that wish to skip evaluation; adapters MUST reject it instead of silently
// allowing.
type Limit struct {
	Window time.Duration
	Max    int
}

// IsZero reports whether l carries no configured budget.
func (l Limit) IsZero() bool { return l == Limit{} }

// Decision is the outcome of a single Check call.
//
//   - Allowed   true → the event was admitted; counters were incremented.
//   - Allowed   false → the event was denied; the caller should refuse the
//     request and surface Retry to the user.
//   - Remaining is the headroom left in the bucket *after* this Check, never
//     negative. On a denied call it is zero.
//   - Retry is the time the caller should wait before retrying. On an
//     allowed call this is the time until the bucket fully resets, which
//     the HTTP layer surfaces as `X-RateLimit-Reset`.
type Decision struct {
	Allowed   bool
	Remaining int
	Retry     time.Duration
}

// Limiter is the port. Adapters MUST be safe for concurrent use.
//
// Check increments the counter for key under limit and returns the resulting
// decision. The key is opaque to the adapter; callers compose it from
// endpoint + bucket-name + bucket-value (typically through middleware so
// that the bucket-value is hashed before logging).
//
// Errors returned by Check signal that the limiter could not reach a
// definitive answer (e.g. Redis is down). They MUST wrap ErrUnavailable so
// that callers can switch to fail-open or fail-closed via errors.Is.
type Limiter interface {
	Check(ctx context.Context, key string, limit Limit) (Decision, error)
}

// ErrUnavailable signals that the limiter could not be consulted (typically a
// transient infrastructure outage). Callers use it with errors.Is to choose
// between fail-open (allow + bypass header) and fail-closed (503).
var ErrUnavailable = errors.New("ratelimit: backend unavailable")

// ErrInvalidLimit signals that the caller asked Check to evaluate a
// zero-value Limit. Adapters return this so that misconfigured rules surface
// during integration tests rather than allowing silently in production.
var ErrInvalidLimit = errors.New("ratelimit: zero Limit not allowed")
