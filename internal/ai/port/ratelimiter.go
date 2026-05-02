// Package port defines AI domain ports — interfaces consumed by AI use-cases
// and implemented by adapters. Domain code MUST NOT import vendor SDKs;
// this file is the surface a use-case sees when it needs a rate limiter.
//
// The shape of RateLimiter and the fail-closed semantics of
// ErrLimiterUnavailable are derived from SIN-62225 §3.6 (ADR 0077 — LLM
// prompt isolation + output handling). See SIN-62238 for the F33
// implementation that introduced this port.
package port

import (
	"context"
	"errors"
	"time"
)

// ErrLimiterUnavailable signals that the rate limiter backend (Redis or any
// future replacement) cannot answer the Allow query — for example because
// the Redis client returned a network error, the Lua script failed, or a
// configured bucket name was rejected.
//
// Per SIN-62225 §3.6 the AI panel rate limiter is fail-closed: any caller
// observing this error MUST treat the request as denied. Concrete adapters
// MUST return Allow=(false, retryAfter > 0, ErrLimiterUnavailable) so a
// middleware that only inspects the boolean still rejects the request.
var ErrLimiterUnavailable = errors.New("ai/ratelimiter: backend unavailable")

// RateLimiter enforces a token-bucket rate limit on the AI panel.
//
// Allow attempts to consume one token from the bucket identified by
// (bucket, key). The bucket name selects a configured policy (capacity +
// refill rate); the key partitions buckets across tenants/users/conv
// pairs. Returns:
//
//   - allowed=true  → call MAY proceed; retryAfter is zero.
//   - allowed=false → call MUST be rejected; retryAfter is the wall-clock
//     duration the caller should wait before retrying.
//
// On backend failure Allow returns (false, retryAfter, ErrLimiterUnavailable);
// retryAfter is a conservative non-zero default (>= 1s) so a caller that
// uses the value verbatim in a Retry-After header is well-defined.
type RateLimiter interface {
	Allow(ctx context.Context, bucket, key string) (allowed bool, retryAfter time.Duration, err error)
}
