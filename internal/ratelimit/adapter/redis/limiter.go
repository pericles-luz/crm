// Package redis is the production rate-limit adapter. It wraps
// github.com/go-redis/redis_rate/v10 and translates redis_rate.Result into
// the ratelimit.Decision contract from ADR 0081 §3 (SIN-62245).
//
// This is the only place under internal/ratelimit/ allowed to import
// github.com/redis/go-redis/v9 or redis_rate; the port-and-adapter rule
// keeps the domain free of redis types so storage can swap without
// touching the use cases.
package redis

import (
	"context"
	"fmt"

	rate "github.com/go-redis/redis_rate/v10"
	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/ratelimit"
)

// Rediser is the narrow subset of *goredis.Client (and Cluster/Ring) that
// redis_rate.NewLimiter requires. Declaring it here lets tests and callers
// substitute a fake without dragging in the full client surface.
//
// The shape mirrors redis_rate's internal interface exactly.
type Rediser interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) *goredis.Cmd
	EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) *goredis.Cmd
	ScriptExists(ctx context.Context, hashes ...string) *goredis.BoolSliceCmd
	ScriptLoad(ctx context.Context, script string) *goredis.StringCmd
	Del(ctx context.Context, keys ...string) *goredis.IntCmd

	EvalRO(ctx context.Context, script string, keys []string, args ...any) *goredis.Cmd
	EvalShaRO(ctx context.Context, sha1 string, keys []string, args ...any) *goredis.Cmd
}

// Limiter wraps a redis_rate.Limiter and renders the redis_rate.Result back
// into the port's Decision contract.
type Limiter struct {
	inner *rate.Limiter
}

// New returns a Limiter that uses rdb for the underlying GCRA script. The
// caller owns rdb's lifecycle.
func New(rdb Rediser) *Limiter {
	return &Limiter{inner: rate.NewLimiter(rdb)}
}

// Check implements ratelimit.Limiter.
//
// Mapping from redis_rate.Result → ratelimit.Decision:
//
//   - Allowed:   res.Allowed > 0
//   - Remaining: res.Remaining (zero on denial)
//   - Retry:     res.RetryAfter when denied, res.ResetAfter when allowed
//
// Errors from redis_rate (network failure, EVALSHA, …) are wrapped with
// ratelimit.ErrUnavailable so middleware can decide between fail-open
// (ADR 0081 §5 default for /api/* GETs) and fail-closed (login, 2FA, etc.).
func (l *Limiter) Check(ctx context.Context, key string, limit ratelimit.Limit) (ratelimit.Decision, error) {
	if limit.IsZero() {
		return ratelimit.Decision{}, ratelimit.ErrInvalidLimit
	}
	res, err := l.inner.Allow(ctx, key, rate.Limit{
		Rate:   limit.Max,
		Burst:  limit.Max,
		Period: limit.Window,
	})
	if err != nil {
		return ratelimit.Decision{}, fmt.Errorf("ratelimit/adapter/redis: %w: %w", ratelimit.ErrUnavailable, err)
	}
	if res.Allowed > 0 {
		return ratelimit.Decision{
			Allowed:   true,
			Remaining: res.Remaining,
			Retry:     res.ResetAfter,
		}, nil
	}
	return ratelimit.Decision{
		Allowed:   false,
		Remaining: 0,
		Retry:     res.RetryAfter,
	}, nil
}

// Compile-time guard that *Limiter implements ratelimit.Limiter.
var _ ratelimit.Limiter = (*Limiter)(nil)
