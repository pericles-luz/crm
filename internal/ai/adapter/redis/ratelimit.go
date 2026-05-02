// Package redis is the Redis-backed adapter for AI ports — currently
// port.RateLimiter. The implementation is a fixed-policy token bucket
// evaluated atomically inside Redis via a Lua script, so concurrent
// requests for the same (bucket, key) pair cannot race each other.
//
// Bucket policies are intentionally hard-coded (not configurable from the
// caller) to satisfy SIN-62225 §3.6: the AI panel limits are a security
// boundary, not a tunable. New buckets require an ADR change.
package redis

import (
	"context"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	aiport "github.com/pericles-luz/crm/internal/ai/port"
)

// Bucket name constants. The HTTP middleware in
// internal/http/middleware/ratelimit consumes these as plain strings so it
// stays decoupled from the adapter package.
const (
	BucketUserConv = "ai:panel:user-conv" // 1 req / 30s, key = tenant:{t}:user:{u}:conv:{c}
	BucketUser     = "ai:panel:user"      // 10 req / 60s, key = tenant:{t}:user:{u}
)

// failClosedRetryAfter is the Retry-After value returned when the backend
// is unavailable. Long enough to discourage retry storms during a Redis
// outage, short enough to recover quickly once Redis comes back.
const failClosedRetryAfter = 5 * time.Second

// bucketConfig captures the token-bucket parameters for one named bucket.
// rateMs is tokens added per millisecond — derived once at compile time
// from the human-friendly "N requests per W seconds" spec.
type bucketConfig struct {
	capacity float64
	rateMs   float64
}

// bucketConfigs is the closed set of buckets this adapter supports. Adding
// a new entry is an ADR change (SIN-62225 §3.6).
var bucketConfigs = map[string]bucketConfig{
	BucketUserConv: {capacity: 1, rateMs: 1.0 / 30000.0},
	BucketUser:     {capacity: 10, rateMs: 10.0 / 60000.0},
}

// scriptSource is the atomic token-bucket evaluation. KEYS[1] is the Redis
// hash key; ARGV is (capacity, rateMs, nowMs, ttlMs). Returns
// {allowed (0|1), retry_after_ms (integer)}.
const scriptSource = `
local capacity = tonumber(ARGV[1])
local rate_ms  = tonumber(ARGV[2])
local now_ms   = tonumber(ARGV[3])
local ttl_ms   = tonumber(ARGV[4])

local data   = redis.call("HMGET", KEYS[1], "tokens", "last_ms")
local tokens = tonumber(data[1])
local last   = tonumber(data[2])

if tokens == nil or last == nil then
  tokens = capacity
  last   = now_ms
else
  local elapsed = now_ms - last
  if elapsed > 0 then
    tokens = math.min(capacity, tokens + elapsed * rate_ms)
    last   = now_ms
  end
end

local allowed = 0
local retry_after_ms = 0
if tokens >= 1 then
  tokens  = tokens - 1
  allowed = 1
else
  local needed = 1 - tokens
  retry_after_ms = math.ceil(needed / rate_ms)
end

redis.call("HMSET", KEYS[1], "tokens", tostring(tokens), "last_ms", tostring(last))
redis.call("PEXPIRE", KEYS[1], ttl_ms)

return {allowed, retry_after_ms}
`

var luaScript = goredis.NewScript(scriptSource)

// Limiter implements port.RateLimiter against Redis. Construct it with
// NewLimiter. Safe for concurrent use.
type Limiter struct {
	client goredis.Scripter
	now    func() time.Time
}

// NewLimiter builds a Limiter wired to the supplied go-redis client. The
// returned value is safe for concurrent use by multiple goroutines.
func NewLimiter(client *goredis.Client) *Limiter {
	return &Limiter{client: client, now: time.Now}
}

// newWithScripter is the test seam used by the package's own unit tests so
// they can drive the limiter against miniredis or a fake; it stays
// unexported to keep the public API intentionally minimal.
func newWithScripter(client goredis.Scripter, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{client: client, now: now}
}

var _ aiport.RateLimiter = (*Limiter)(nil)

// Allow consumes one token from (bucket, key). See port.RateLimiter for the
// full contract. Unknown bucket names and empty keys produce
// ErrLimiterUnavailable so a misconfiguration fails closed.
func (l *Limiter) Allow(ctx context.Context, bucket, key string) (bool, time.Duration, error) {
	cfg, ok := bucketConfigs[bucket]
	if !ok {
		return false, failClosedRetryAfter, fmt.Errorf("%w: unknown bucket %q", aiport.ErrLimiterUnavailable, bucket)
	}
	if key == "" {
		return false, failClosedRetryAfter, fmt.Errorf("%w: empty key for bucket %q", aiport.ErrLimiterUnavailable, bucket)
	}

	redisKey := bucket + ":" + key
	nowMs := l.now().UnixMilli()
	// Stale-key TTL: 2 full refills. Long enough to keep state across a
	// burst, short enough to release memory for inactive (user, conv) pairs.
	ttlMs := int64((cfg.capacity / cfg.rateMs) * 2)
	if ttlMs <= 0 {
		ttlMs = 60_000
	}

	res, err := luaScript.Run(ctx, l.client, []string{redisKey},
		cfg.capacity, cfg.rateMs, nowMs, ttlMs).Result()
	if err != nil {
		return false, failClosedRetryAfter, fmt.Errorf("%w: %v", aiport.ErrLimiterUnavailable, err)
	}

	allowed, retryAfter, parseErr := parseScriptResult(res)
	if parseErr != nil {
		return false, failClosedRetryAfter, fmt.Errorf("%w: %v", aiport.ErrLimiterUnavailable, parseErr)
	}
	return allowed, retryAfter, nil
}

// parseScriptResult decodes the {allowed, retry_after_ms} reply.
func parseScriptResult(res any) (bool, time.Duration, error) {
	arr, ok := res.([]any)
	if !ok || len(arr) != 2 {
		return false, 0, errors.New("unexpected script reply shape")
	}
	allowedInt, err := toInt64(arr[0])
	if err != nil {
		return false, 0, fmt.Errorf("allowed: %w", err)
	}
	retryMs, err := toInt64(arr[1])
	if err != nil {
		return false, 0, fmt.Errorf("retryAfter: %w", err)
	}
	if retryMs < 0 {
		retryMs = 0
	}
	return allowedInt == 1, time.Duration(retryMs) * time.Millisecond, nil
}

func toInt64(v any) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	case float64:
		return int64(x), nil
	case string:
		var n int64
		if _, err := fmt.Sscanf(x, "%d", &n); err != nil {
			return 0, err
		}
		return n, nil
	default:
		return 0, fmt.Errorf("unexpected type %T", v)
	}
}
