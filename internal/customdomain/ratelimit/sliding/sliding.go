// Package sliding implements the per-host sliding-window rate limiter the
// /internal/tls/ask handler consults (SIN-62243 F45 deliverable 2). It
// satisfies the tls_ask.RateLimiter port.
//
// Algorithm: Redis sorted-set sliding window driven by a single Lua
// script per call. The script atomically:
//
//  1. ZREMRANGEBYSCORE bucket 0 cutoff   -- drop entries older than the window.
//  2. ZADD bucket now member             -- record this call.
//  3. ZCARD bucket                       -- count entries in window.
//  4. EXPIRE bucket ttl                  -- bound key lifetime to the window.
//  5. return ZCARD                       -- caller compares against max.
//
// Driving the four commands from one Lua call gives us atomicity (no
// TOCTTOU between concurrent callers) AND a one-method redis port surface
// (Eval), which keeps the unit-test fake small and on the right side of
// the CTO's "no mocking the database" rule for adapters: tests use a
// purpose-built fake of the redis Eval semantic, not a behaviour-changing
// mock.
package sliding

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Cmdable is the narrow surface of the go-redis client this adapter uses.
// One method, intentionally — see the package doc for why.
type Cmdable interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) *goredis.Cmd
}

// Limiter is the sliding-window adapter. Construct with New and reuse;
// safe for concurrent use.
type Limiter struct {
	client    Cmdable
	keyPrefix string
	max       int64         // ceiling, e.g. 3 (4th call denies)
	window    time.Duration // window length, e.g. 1 minute
	rng       func() (string, error)
}

// SetMember overrides the unique-member generator. Test-only hook; nothing
// in production calls this. Exposed so the rng-error branch in Allow can
// be exercised without monkey-patching crypto/rand.
func (l *Limiter) SetMember(fn func() (string, error)) {
	if fn != nil {
		l.rng = fn
	}
}

// New builds a Limiter. keyPrefix namespaces the Redis keys; max is the
// inclusive cap (calls 1..max return Allow=true; call max+1 returns false);
// window is the rolling window length.
func New(client Cmdable, keyPrefix string, max int, window time.Duration) *Limiter {
	if keyPrefix == "" {
		keyPrefix = "customdomain:tls_ask"
	}
	if max <= 0 {
		max = 3 // F45 spec default: 3 lookups/min/host.
	}
	if window <= 0 {
		window = time.Minute
	}
	return &Limiter{
		client:    client,
		keyPrefix: keyPrefix,
		max:       int64(max),
		window:    window,
		rng:       uniqueMember,
	}
}

// luaScript runs the four ZSET ops atomically and returns the post-ZADD
// member count. KEYS[1]=bucket, ARGV[1]=now-score, ARGV[2]=cutoff-score,
// ARGV[3]=member, ARGV[4]=ttl-seconds.
const luaScript = `
local key = KEYS[1]
local now = tonumber(ARGV[1])
local cutoff = tonumber(ARGV[2])
local member = ARGV[3]
local ttl = tonumber(ARGV[4])
redis.call('ZREMRANGEBYSCORE', key, 0, cutoff)
redis.call('ZADD', key, now, member)
local count = redis.call('ZCARD', key)
redis.call('EXPIRE', key, ttl)
return count
`

// Allow records the call and returns true iff the rolling window count
// is at or below the configured max.
//
// Errors propagate (not silenced); the caller fails closed.
func (l *Limiter) Allow(ctx context.Context, host string, now time.Time) (bool, error) {
	if host == "" {
		return false, errors.New("sliding: empty host")
	}
	key := l.keyPrefix + ":" + host
	score := now.UnixNano()
	cutoff := now.Add(-l.window).UnixNano()
	ttl := int64(l.window/time.Second) + 5 // small grace beyond window
	member, err := l.rng()
	if err != nil {
		return false, fmt.Errorf("sliding: random member: %w", err)
	}

	res, err := l.client.Eval(ctx, luaScript, []string{key}, score, cutoff, member, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("sliding: eval: %w", err)
	}
	count, err := asInt64(res)
	if err != nil {
		return false, fmt.Errorf("sliding: count: %w", err)
	}
	return count <= l.max, nil
}

// uniqueMember returns 16 random bytes as hex. Used as the ZSET member id.
// Good enough to avoid collisions even at 10⁶ calls/sec.
func uniqueMember() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// asInt64 normalises whatever the redis client returns from EVAL: real
// servers return int64, miniredis returns int, RESP3 may return int (the
// goredis driver normalises to int64 in v9 but we accept both for
// safety).
func asInt64(v any) (int64, error) {
	switch t := v.(type) {
	case int64:
		return t, nil
	case int:
		return int64(t), nil
	default:
		return 0, fmt.Errorf("unexpected eval return type %T", v)
	}
}
