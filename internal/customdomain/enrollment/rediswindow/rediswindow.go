// Package rediswindow implements enrollment.WindowCounter against Redis
// using a sliding-window sorted-set per (tenantID, window) key. It is the
// production swap for the dev placeholders documented in
// cmd/server/customdomain_wire.go (SIN-62334 F53): until this adapter is
// wired, CUSTOM_DOMAIN_UI_ENABLED=1 in production would bypass the F45
// per-tenant enrollment quotas and re-open the LE-DoS attack surface.
//
// Algorithm (one Lua call per CountAndRecord, keeping it atomic):
//
//  1. ZREMRANGEBYSCORE bucket 0 cutoff   -- drop entries older than the window.
//  2. ZADD bucket now member             -- record this call (unique random id).
//  3. ZCARD bucket                       -- count entries in window.
//  4. EXPIRE bucket ttl                  -- bound key lifetime to the window+grace.
//  5. return ZCARD                       -- caller compares against the quota.
//
// The TTL is set to the window + small grace so abandoned tenants stop
// taking up Redis memory after their longest window expires.
//
// Key layout:
//
//	customdomain:enrollment:{tenantID}:hour
//	customdomain:enrollment:{tenantID}:day
//	customdomain:enrollment:{tenantID}:month
//
// Idempotency on retry: every call uses a unique random member id, so
// retrying a failed CountAndRecord cannot double-count when the only
// failure mode is a network round-trip after Redis already executed the
// Lua script. The +1 over-count enrollment.UseCase tolerates is bounded
// by the number of denied calls, not retries.
package rediswindow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/customdomain/enrollment"
)

// Cmdable is the narrow surface of the go-redis client this adapter uses.
// One method, intentionally — the same fake the sliding-window package
// uses (internal/customdomain/ratelimit/sliding) is reusable here, and
// the small surface keeps the unit-test fake on the right side of the
// CTO "no mocking the database" rule (a documented in-memory adapter
// matching Redis semantics, not a stub returning canned values).
type Cmdable interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) *goredis.Cmd
}

// Counter is the Redis-backed enrollment.WindowCounter. Construct with
// New and reuse; safe for concurrent use as long as the underlying
// Cmdable is.
type Counter struct {
	client    Cmdable
	keyPrefix string
	rng       func() (string, error)
}

// SetMember overrides the unique-member generator. Test-only hook so the
// rng-error branch in CountAndRecord can be exercised without
// monkey-patching crypto/rand.
func (c *Counter) SetMember(fn func() (string, error)) {
	if fn != nil {
		c.rng = fn
	}
}

// New builds a Counter. keyPrefix namespaces the Redis keys; pass
// "customdomain:enrollment" to match the canonical wiring in
// cmd/server/customdomain_wire.go.
func New(client Cmdable, keyPrefix string) *Counter {
	if keyPrefix == "" {
		keyPrefix = "customdomain:enrollment"
	}
	return &Counter{
		client:    client,
		keyPrefix: keyPrefix,
		rng:       uniqueMember,
	}
}

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

// CountAndRecord implements enrollment.WindowCounter.
func (c *Counter) CountAndRecord(ctx context.Context, tenantID uuid.UUID, window enrollment.Window, now time.Time) (int, error) {
	if tenantID == uuid.Nil {
		return 0, errors.New("rediswindow: tenantID must not be uuid.Nil")
	}
	key := c.bucketKey(tenantID, window)
	dur := window.Duration()
	score := now.UnixNano()
	cutoff := now.Add(-dur).UnixNano()
	ttl := int64(dur/time.Second) + 60 // grace beyond the window
	member, err := c.rng()
	if err != nil {
		return 0, fmt.Errorf("rediswindow: random member: %w", err)
	}

	res, err := c.client.Eval(ctx, luaScript, []string{key}, score, cutoff, member, ttl).Result()
	if err != nil {
		return 0, fmt.Errorf("rediswindow: eval: %w", err)
	}
	count, err := asInt(res)
	if err != nil {
		return 0, fmt.Errorf("rediswindow: count: %w", err)
	}
	return count, nil
}

func (c *Counter) bucketKey(tenantID uuid.UUID, w enrollment.Window) string {
	return c.keyPrefix + ":" + tenantID.String() + ":" + w.String()
}

// uniqueMember returns 16 random bytes as hex. Used as the ZSET member
// id; collisions at this length are negligible even at 10⁶ calls/sec.
func uniqueMember() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// asInt normalises Redis EVAL return types: real servers return int64,
// miniredis returns int, RESP3 keeps int64. Anything else surfaces as a
// clear error so a future driver change cannot silently degrade to an
// allow-all counter.
func asInt(v any) (int, error) {
	switch t := v.(type) {
	case int64:
		return int(t), nil
	case int:
		return t, nil
	default:
		return 0, fmt.Errorf("unexpected eval return type %T", v)
	}
}

var _ enrollment.WindowCounter = (*Counter)(nil)
