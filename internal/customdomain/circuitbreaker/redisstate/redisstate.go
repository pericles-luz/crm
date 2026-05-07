// Package redisstate implements circuitbreaker.State against Redis. It is
// the production swap for the in-memory placeholder (zeroBreaker) wired
// in cmd/server/customdomain_wire.go (SIN-62334 F53). Without this
// adapter, an attacker who fails enough LE issuances on one replica
// could keep hammering the upstream from a sibling replica that shares
// no in-process breaker state — the breaker only stops issuance on the
// replica that observed the failures.
//
// Atomicity comes from one Lua call per RecordFailure (same pattern as
// internal/customdomain/ratelimit/sliding and
// internal/customdomain/enrollment/rediswindow). Trip / IsOpen / Reset
// are single-command Lua wrappers so the Cmdable port surface stays
// one method (Eval), matching the rest of the codebase and keeping the
// unit-test fake small.
//
// Persistence: all keys carry TTLs that outlive the configured window /
// freeze, so a tenant who never recovers does not pin Redis memory.
// The 24h freeze survives server restarts naturally because Redis is
// the source of truth.
//
// Key layout (one ZSET + one string per tenant):
//
//	customdomain:lebreaker:fail:{tenantID}    -- ZSET, member = unique id, score = nanos
//	customdomain:lebreaker:frozen:{tenantID}  -- STRING, TTL = freezeFor; presence = open
package redisstate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/customdomain/circuitbreaker"
)

// Cmdable is the narrow surface of go-redis this adapter uses: one
// method (Eval). Same shape as ratelimit/sliding — see the package
// doc there for why the fake stays out of the "no mocking the
// database" trap (it matches Redis semantics, not stubs canned values).
type Cmdable interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) *goredis.Cmd
}

// State is the Redis-backed circuitbreaker.State. Construct with New
// and reuse; safe for concurrent use as long as the Cmdable is.
type State struct {
	client    Cmdable
	keyPrefix string
	rng       func() (string, error)
}

// SetMember overrides the unique-member generator. Test-only hook so
// the rng-error branch is exercisable without monkey-patching crypto/rand.
func (s *State) SetMember(fn func() (string, error)) {
	if fn != nil {
		s.rng = fn
	}
}

// New builds a State. keyPrefix namespaces the Redis keys; pass
// "customdomain:lebreaker" to match the canonical wiring.
func New(client Cmdable, keyPrefix string) *State {
	if keyPrefix == "" {
		keyPrefix = "customdomain:lebreaker"
	}
	return &State{
		client:    client,
		keyPrefix: keyPrefix,
		rng:       uniqueMember,
	}
}

const recordFailureScript = `
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

const tripScript = `
local key = KEYS[1]
local val = ARGV[1]
local freeze_ms = tonumber(ARGV[2])
redis.call('SET', key, val, 'PX', freeze_ms)
return 1
`

const isOpenScript = `
return redis.call('EXISTS', KEYS[1])
`

const resetScript = `
redis.call('DEL', KEYS[1], KEYS[2])
return 1
`

// RecordFailure implements circuitbreaker.State. ZSET sliding window:
// add (now, unique-id), drop entries older than window, return count.
func (s *State) RecordFailure(ctx context.Context, tenantID uuid.UUID, _ string, now time.Time, window time.Duration) (int, error) {
	if tenantID == uuid.Nil {
		return 0, errors.New("redisstate: tenantID must not be uuid.Nil")
	}
	if window <= 0 {
		return 0, errors.New("redisstate: window must be positive")
	}
	key := s.failKey(tenantID)
	score := now.UnixNano()
	cutoff := now.Add(-window).UnixNano()
	ttl := int64(window/time.Second) + 60 // grace beyond the window
	member, err := s.rng()
	if err != nil {
		return 0, fmt.Errorf("redisstate: random member: %w", err)
	}

	res, err := s.client.Eval(ctx, recordFailureScript, []string{key}, score, cutoff, member, ttl).Result()
	if err != nil {
		return 0, fmt.Errorf("redisstate: record failure: %w", err)
	}
	count, err := asInt(res)
	if err != nil {
		return 0, fmt.Errorf("redisstate: record failure: %w", err)
	}
	return count, nil
}

// Trip implements circuitbreaker.State. SET frozen-key with PX TTL =
// freezeFor; idempotent. The value is the unix-nanos deadline (kept
// for diagnostics; presence of the key drives IsOpen).
func (s *State) Trip(ctx context.Context, tenantID uuid.UUID, now time.Time, freezeFor time.Duration) error {
	if tenantID == uuid.Nil {
		return errors.New("redisstate: tenantID must not be uuid.Nil")
	}
	if freezeFor <= 0 {
		return errors.New("redisstate: freezeFor must be positive")
	}
	key := s.frozenKey(tenantID)
	deadline := now.Add(freezeFor).UnixNano()
	freezeMS := freezeFor.Milliseconds()
	if freezeMS <= 0 {
		freezeMS = 1
	}
	if _, err := s.client.Eval(ctx, tripScript, []string{key}, deadline, freezeMS).Result(); err != nil {
		return fmt.Errorf("redisstate: trip: %w", err)
	}
	return nil
}

// IsOpen implements circuitbreaker.State. Returns true while the
// frozen-key exists; the TTL on Trip removes the key when the freeze
// elapses, so we do not need to compare timestamps in the adapter.
func (s *State) IsOpen(ctx context.Context, tenantID uuid.UUID, _ time.Time) (bool, error) {
	if tenantID == uuid.Nil {
		return false, errors.New("redisstate: tenantID must not be uuid.Nil")
	}
	key := s.frozenKey(tenantID)
	res, err := s.client.Eval(ctx, isOpenScript, []string{key}).Result()
	if err != nil {
		return false, fmt.Errorf("redisstate: is open: %w", err)
	}
	v, err := asInt(res)
	if err != nil {
		return false, fmt.Errorf("redisstate: is open: %w", err)
	}
	return v == 1, nil
}

// Reset implements circuitbreaker.State. DELs both keys atomically;
// idempotent (DEL on missing key is a no-op).
func (s *State) Reset(ctx context.Context, tenantID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return errors.New("redisstate: tenantID must not be uuid.Nil")
	}
	keys := []string{s.failKey(tenantID), s.frozenKey(tenantID)}
	if _, err := s.client.Eval(ctx, resetScript, keys).Result(); err != nil {
		return fmt.Errorf("redisstate: reset: %w", err)
	}
	return nil
}

func (s *State) failKey(tenantID uuid.UUID) string {
	return s.keyPrefix + ":fail:" + tenantID.String()
}

func (s *State) frozenKey(tenantID uuid.UUID) string {
	return s.keyPrefix + ":frozen:" + tenantID.String()
}

// uniqueMember returns 16 random bytes as hex; collision-free at any
// realistic call rate.
func uniqueMember() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// asInt normalises Redis EVAL return types: real servers return int64,
// miniredis returns int. Anything else is a clear error so a future
// driver change cannot silently degrade.
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

var _ circuitbreaker.State = (*State)(nil)
