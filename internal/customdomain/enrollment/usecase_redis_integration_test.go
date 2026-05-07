package enrollment_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/customdomain/circuitbreaker"
	"github.com/pericles-luz/crm/internal/customdomain/circuitbreaker/redisstate"
	"github.com/pericles-luz/crm/internal/customdomain/enrollment"
	"github.com/pericles-luz/crm/internal/customdomain/enrollment/rediswindow"
)

// SIN-62334 F53 / AC #4 — wire enrollment.UseCase against the same
// Redis-backed adapters production uses (rediswindow + redisstate) +
// a static CountStore stub for the 25-active hard cap, and assert the
// documented thresholds trip and the breaker freezes/resets correctly
// across a simulated restart. The fake-Redis server matches the
// sliding-window adapter's documented in-memory pattern (CTO-approved
// alternative to testcontainers; same semantics, no new dep).

type fakeRedisServer struct {
	mu       sync.Mutex
	now      time.Time
	zsets    map[string][]zentry
	keys     map[string]string
	expiries map[string]time.Time
}

type zentry struct {
	score  int64
	member string
}

func newFakeRedis(initial time.Time) *fakeRedisServer {
	return &fakeRedisServer{
		now:      initial,
		zsets:    map[string][]zentry{},
		keys:     map[string]string{},
		expiries: map[string]time.Time{},
	}
}

func (f *fakeRedisServer) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func (f *fakeRedisServer) Eval(_ context.Context, script string, keys []string, args ...any) *goredis.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := &goredis.Cmd{}
	for k, deadline := range f.expiries {
		if !f.now.Before(deadline) {
			delete(f.zsets, k)
			delete(f.keys, k)
			delete(f.expiries, k)
		}
	}
	switch {
	case strings.Contains(script, "ZADD"):
		key := keys[0]
		score, _ := toI64(args[0])
		cutoff, _ := toI64(args[1])
		member := args[2].(string)
		ttl, _ := toI64(args[3])
		entries := f.zsets[key]
		kept := entries[:0]
		for _, e := range entries {
			if e.score > cutoff {
				kept = append(kept, e)
			}
		}
		kept = append(kept, zentry{score: score, member: member})
		f.zsets[key] = kept
		f.expiries[key] = f.now.Add(time.Duration(ttl) * time.Second)
		cmd.SetVal(int64(len(kept)))
	case strings.Contains(script, "EXISTS"):
		if _, ok := f.keys[keys[0]]; ok {
			cmd.SetVal(int64(1))
			return cmd
		}
		cmd.SetVal(int64(0))
	case strings.Contains(script, "DEL"):
		for _, k := range keys {
			delete(f.keys, k)
			delete(f.zsets, k)
			delete(f.expiries, k)
		}
		cmd.SetVal(int64(1))
	case strings.Contains(script, "SET"):
		key := keys[0]
		val, _ := toI64(args[0])
		freezeMs, _ := toI64(args[1])
		f.keys[key] = strconv.FormatInt(val, 10)
		f.expiries[key] = f.now.Add(time.Duration(freezeMs) * time.Millisecond)
		cmd.SetVal(int64(1))
	default:
		cmd.SetErr(errors.New("fake: unknown script"))
	}
	return cmd
}

func (f *fakeRedisServer) Close() error { return nil }

func toI64(v any) (int64, error) {
	switch t := v.(type) {
	case int:
		return int64(t), nil
	case int64:
		return t, nil
	case string:
		return strconv.ParseInt(t, 10, 64)
	}
	return 0, errors.New("not numeric")
}

// staticCountStore satisfies enrollment.CountStore by always returning
// the configured value. The 25-active hard cap is database-sourced in
// production (Postgres COUNT(*)), so for the threshold integration we
// just return a fixed count rather than spinning up a Postgres
// testcontainer (which the project does not depend on yet).
type staticCountStore struct{ n int }

func (s staticCountStore) ActiveCount(_ context.Context, _ uuid.UUID) (int, error) {
	return s.n, nil
}

// breakerAdapter mirrors cmd/server.breakerAdapter — projects the
// circuitbreaker.UseCase into enrollment.Breaker.
type breakerAdapter struct{ uc *circuitbreaker.UseCase }

func (b breakerAdapter) IsOpen(ctx context.Context, tenantID uuid.UUID, now time.Time) (bool, error) {
	return b.uc.IsOpen(ctx, tenantID, now)
}

// fixedClock returns a stable wall-clock so the tests are deterministic.
func fixedClock(t time.Time) enrollment.Clock { return func() time.Time { return t } }

// buildIntegrationGate wires the use-case the same way production
// would (minus the Postgres CountStore — staticCountStore stands in
// for the indexed COUNT(*)).
func buildIntegrationGate(t *testing.T, redis *fakeRedisServer, active int, now time.Time) *enrollment.UseCase {
	t.Helper()
	store := staticCountStore{n: active}
	winCounter := rediswindow.New(redis, "customdomain:enrollment")
	bState := redisstate.New(redis, "customdomain:lebreaker")
	breaker := circuitbreaker.New(bState, nil, func() time.Time { return now }, circuitbreaker.DefaultConfig())
	return enrollment.New(store, winCounter, breakerAdapter{uc: breaker}, nil, fixedClock(now), enrollment.DefaultQuota())
}

func TestIntegration_HourlyQuotaTripsAt6thCall(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	redis := newFakeRedis(now)
	uc := buildIntegrationGate(t, redis, 0, now)
	tenant := uuid.New()

	for i := 1; i <= 5; i++ {
		got := uc.Allow(context.Background(), tenant)
		if got.Decision != enrollment.DecisionAllowed {
			t.Fatalf("call %d: decision=%s, want allowed", i, got.Decision)
		}
	}
	got := uc.Allow(context.Background(), tenant)
	if got.Decision != enrollment.DecisionDeniedHourlyQuota {
		t.Fatalf("6th call: decision=%s, want hourly_quota", got.Decision)
	}
}

func TestIntegration_DailyQuotaTripsAt21stCall(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
	redis := newFakeRedis(now)
	tenant := uuid.New()

	// 4 hours x 5 attempts = 20 allowed (each hour resets the hourly
	// window). Only Daily ≤ 20 is the binding constraint at the 21st
	// call when we move to a 5th hour.
	for hour := 0; hour < 4; hour++ {
		base := now.Add(time.Duration(hour) * time.Hour)
		redis.advance(time.Hour)
		clock := func() time.Time { return base }
		store := staticCountStore{n: 0}
		winCounter := rediswindow.New(redis, "customdomain:enrollment")
		bState := redisstate.New(redis, "customdomain:lebreaker")
		breaker := circuitbreaker.New(bState, nil, clock, circuitbreaker.DefaultConfig())
		hourlyUC := enrollment.New(store, winCounter, breakerAdapter{uc: breaker}, nil, clock, enrollment.DefaultQuota())
		for i := 0; i < 5; i++ {
			got := hourlyUC.Allow(context.Background(), tenant)
			if got.Decision != enrollment.DecisionAllowed {
				t.Fatalf("hour %d call %d: decision=%s, want allowed", hour, i+1, got.Decision)
			}
		}
	}
	// Hour 4 → 21st call: hourly is fresh (1/5), daily is at 21/20 → deny.
	hour4 := now.Add(4 * time.Hour)
	redis.advance(time.Hour)
	clock := func() time.Time { return hour4 }
	store := staticCountStore{n: 0}
	winCounter := rediswindow.New(redis, "customdomain:enrollment")
	bState := redisstate.New(redis, "customdomain:lebreaker")
	breaker := circuitbreaker.New(bState, nil, clock, circuitbreaker.DefaultConfig())
	hourlyUC := enrollment.New(store, winCounter, breakerAdapter{uc: breaker}, nil, clock, enrollment.DefaultQuota())
	got := hourlyUC.Allow(context.Background(), tenant)
	if got.Decision != enrollment.DecisionDeniedDailyQuota {
		t.Fatalf("21st call: decision=%s, want daily_quota", got.Decision)
	}
}

func TestIntegration_HardCapTripsAt25Active(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	redis := newFakeRedis(now)
	uc := buildIntegrationGate(t, redis, 25, now)
	tenant := uuid.New()
	got := uc.Allow(context.Background(), tenant)
	if got.Decision != enrollment.DecisionDeniedHardCap {
		t.Fatalf("25 active: decision=%s, want hard_cap", got.Decision)
	}
}

func TestIntegration_HardCapAllowsBelowThreshold(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	redis := newFakeRedis(now)
	uc := buildIntegrationGate(t, redis, 24, now)
	tenant := uuid.New()
	got := uc.Allow(context.Background(), tenant)
	if got.Decision != enrollment.DecisionAllowed {
		t.Fatalf("24 active: decision=%s, want allowed", got.Decision)
	}
}

func TestIntegration_BreakerTripsFreezesAndResetsAcrossRestart(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	redis := newFakeRedis(now)
	tenant := uuid.New()

	// Trip the breaker via the State port directly (the breaker
	// use-case is what the gate uses; we drive it via a focused
	// circuitbreaker.UseCase here to keep the test on-topic).
	bState := redisstate.New(redis, "customdomain:lebreaker")
	breakerUC := circuitbreaker.New(bState, nil, func() time.Time { return now }, circuitbreaker.DefaultConfig())
	for i := 0; i < 5; i++ {
		if _, err := breakerUC.RecordFailure(context.Background(), tenant, "shop.example.com"); err != nil {
			t.Fatalf("RecordFailure %d: %v", i, err)
		}
	}

	// Now wire the gate; the breaker should be frozen → DenyCircuitBreaker.
	uc := buildIntegrationGate(t, redis, 0, now)
	got := uc.Allow(context.Background(), tenant)
	if got.Decision != enrollment.DecisionDeniedCircuitBreaker {
		t.Fatalf("post-trip: decision=%s, want circuit_breaker", got.Decision)
	}

	// Simulate process restart: rebuild State on the same Redis. The
	// freeze must persist (Redis is the source of truth).
	uc2 := buildIntegrationGate(t, redis, 0, now)
	got = uc2.Allow(context.Background(), tenant)
	if got.Decision != enrollment.DecisionDeniedCircuitBreaker {
		t.Fatalf("post-restart: decision=%s, want still circuit_breaker", got.Decision)
	}

	// 24h + 1m later: TTL on the frozen key has elapsed → breaker open=false.
	redis.advance(24*time.Hour + time.Minute)
	later := now.Add(24*time.Hour + time.Minute)
	uc3 := func() *enrollment.UseCase {
		store := staticCountStore{n: 0}
		winCounter := rediswindow.New(redis, "customdomain:enrollment")
		bs := redisstate.New(redis, "customdomain:lebreaker")
		bk := circuitbreaker.New(bs, nil, func() time.Time { return later }, circuitbreaker.DefaultConfig())
		return enrollment.New(store, winCounter, breakerAdapter{uc: bk}, nil, fixedClock(later), enrollment.DefaultQuota())
	}()
	got = uc3.Allow(context.Background(), tenant)
	if got.Decision != enrollment.DecisionAllowed {
		t.Fatalf("post-freeze: decision=%s, want allowed", got.Decision)
	}
}

func TestIntegration_SuccessfulIssuanceResetsBreaker(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	redis := newFakeRedis(now)
	tenant := uuid.New()

	bState := redisstate.New(redis, "customdomain:lebreaker")
	breakerUC := circuitbreaker.New(bState, nil, func() time.Time { return now }, circuitbreaker.DefaultConfig())
	for i := 0; i < 5; i++ {
		if _, err := breakerUC.RecordFailure(context.Background(), tenant, "shop.example.com"); err != nil {
			t.Fatalf("RecordFailure %d: %v", i, err)
		}
	}
	if err := breakerUC.RecordSuccess(context.Background(), tenant); err != nil {
		t.Fatalf("RecordSuccess: %v", err)
	}

	uc := buildIntegrationGate(t, redis, 0, now)
	got := uc.Allow(context.Background(), tenant)
	if got.Decision != enrollment.DecisionAllowed {
		t.Fatalf("post-RecordSuccess: decision=%s, want allowed", got.Decision)
	}
}
