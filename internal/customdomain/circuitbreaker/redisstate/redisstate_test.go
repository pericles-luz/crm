package redisstate_test

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
)

// fakeServer is an in-memory Redis emulator that knows enough Lua
// semantics to drive redisstate end-to-end. It dispatches by inspecting
// the script body for the unique command (ZADD / SET / EXISTS / DEL)
// each script issues — same documented-fake pattern as
// internal/customdomain/ratelimit/sliding's test, on the right side of
// the CTO "no mocking the database" rule (matches Redis semantics, not
// canned responses).
type fakeServer struct {
	mu       sync.Mutex
	now      time.Time
	zsets    map[string][]zsetEntry
	strings  map[string]string
	expiries map[string]time.Time
	failNext error
}

type zsetEntry struct {
	score  int64
	member string
}

func newFakeServer(initial time.Time) *fakeServer {
	return &fakeServer{
		now:      initial,
		zsets:    map[string][]zsetEntry{},
		strings:  map[string]string{},
		expiries: map[string]time.Time{},
	}
}

func (f *fakeServer) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func (f *fakeServer) Eval(_ context.Context, script string, keys []string, args ...any) *goredis.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := &goredis.Cmd{}
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		cmd.SetErr(err)
		return cmd
	}
	f.expireDue()
	switch {
	case strings.Contains(script, "ZADD"):
		f.evalRecordFailure(cmd, keys, args)
	case strings.Contains(script, "SET"):
		f.evalTrip(cmd, keys, args)
	case strings.Contains(script, "EXISTS"):
		f.evalIsOpen(cmd, keys)
	case strings.Contains(script, "DEL"):
		f.evalReset(cmd, keys)
	default:
		cmd.SetErr(errors.New("fake: unknown script"))
	}
	return cmd
}

func (f *fakeServer) expireDue() {
	for k, deadline := range f.expiries {
		if !f.now.Before(deadline) {
			delete(f.zsets, k)
			delete(f.strings, k)
			delete(f.expiries, k)
		}
	}
}

func (f *fakeServer) evalRecordFailure(cmd *goredis.Cmd, keys []string, args []any) {
	if len(keys) != 1 || len(args) != 4 {
		cmd.SetErr(errors.New("fake: record-failure arity"))
		return
	}
	key := keys[0]
	score, err := toInt64(args[0])
	if err != nil {
		cmd.SetErr(err)
		return
	}
	cutoff, err := toInt64(args[1])
	if err != nil {
		cmd.SetErr(err)
		return
	}
	member, ok := args[2].(string)
	if !ok {
		cmd.SetErr(errors.New("fake: member must be string"))
		return
	}
	ttl, err := toInt64(args[3])
	if err != nil {
		cmd.SetErr(err)
		return
	}
	entries := f.zsets[key]
	kept := entries[:0]
	for _, e := range entries {
		if e.score > cutoff {
			kept = append(kept, e)
		}
	}
	kept = append(kept, zsetEntry{score: score, member: member})
	f.zsets[key] = kept
	f.expiries[key] = f.now.Add(time.Duration(ttl) * time.Second)
	cmd.SetVal(int64(len(kept)))
}

func (f *fakeServer) evalTrip(cmd *goredis.Cmd, keys []string, args []any) {
	if len(keys) != 1 || len(args) != 2 {
		cmd.SetErr(errors.New("fake: trip arity"))
		return
	}
	key := keys[0]
	val, err := toInt64(args[0])
	if err != nil {
		cmd.SetErr(err)
		return
	}
	freezeMs, err := toInt64(args[1])
	if err != nil {
		cmd.SetErr(err)
		return
	}
	f.strings[key] = strconv.FormatInt(val, 10)
	f.expiries[key] = f.now.Add(time.Duration(freezeMs) * time.Millisecond)
	cmd.SetVal(int64(1))
}

func (f *fakeServer) evalIsOpen(cmd *goredis.Cmd, keys []string) {
	if len(keys) != 1 {
		cmd.SetErr(errors.New("fake: is-open arity"))
		return
	}
	if _, ok := f.strings[keys[0]]; ok {
		cmd.SetVal(int64(1))
		return
	}
	cmd.SetVal(int64(0))
}

func (f *fakeServer) evalReset(cmd *goredis.Cmd, keys []string) {
	for _, k := range keys {
		delete(f.zsets, k)
		delete(f.strings, k)
		delete(f.expiries, k)
	}
	cmd.SetVal(int64(1))
}

func toInt64(v any) (int64, error) {
	switch t := v.(type) {
	case int:
		return int64(t), nil
	case int64:
		return t, nil
	case string:
		return strconv.ParseInt(t, 10, 64)
	default:
		return 0, errors.New("fake: unsupported numeric type")
	}
}

func TestState_RecordFailure_CountsWithinWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(now)
	s := redisstate.New(srv, "")
	tenant := uuid.New()

	for i := 1; i <= 5; i++ {
		got, err := s.RecordFailure(context.Background(), tenant, "shop.example.com", now, time.Hour)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got != i {
			t.Fatalf("call %d: count = %d, want %d", i, got, i)
		}
	}
}

func TestState_RecordFailure_DropsAgedEntries(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(now)
	s := redisstate.New(srv, "")
	tenant := uuid.New()

	if _, err := s.RecordFailure(context.Background(), tenant, "h", now, time.Hour); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv.advance(time.Hour + time.Second)
	got, err := s.RecordFailure(context.Background(), tenant, "h", now.Add(time.Hour+time.Second), time.Hour)
	if err != nil {
		t.Fatalf("post-aged: %v", err)
	}
	if got != 1 {
		t.Fatalf("aged entry not dropped: got %d, want 1", got)
	}
}

func TestState_Trip_IsOpen_FreezeRespected(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(now)
	s := redisstate.New(srv, "")
	tenant := uuid.New()

	if err := s.Trip(context.Background(), tenant, now, 24*time.Hour); err != nil {
		t.Fatalf("trip: %v", err)
	}
	open, err := s.IsOpen(context.Background(), tenant, now)
	if err != nil {
		t.Fatalf("is-open: %v", err)
	}
	if !open {
		t.Fatal("breaker not open immediately after trip")
	}
	// Halfway through the freeze: still open.
	srv.advance(12 * time.Hour)
	open, _ = s.IsOpen(context.Background(), tenant, now.Add(12*time.Hour))
	if !open {
		t.Fatal("breaker should still be open at 12h into a 24h freeze")
	}
	// Past the freeze: TTL expired, key gone, IsOpen=false.
	srv.advance(13 * time.Hour)
	open, _ = s.IsOpen(context.Background(), tenant, now.Add(25*time.Hour))
	if open {
		t.Fatal("breaker should have unfrozen after 24h TTL")
	}
}

func TestState_PersistsAcrossRestart(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(now)
	s := redisstate.New(srv, "")
	tenant := uuid.New()

	if err := s.Trip(context.Background(), tenant, now, 24*time.Hour); err != nil {
		t.Fatalf("trip: %v", err)
	}

	// "Restart": rebuild the State pointing at the same fake (Redis
	// persists across the app process — that's the whole point of using
	// Redis instead of in-memory state for multi-replica deploys).
	s2 := redisstate.New(srv, "")
	open, err := s2.IsOpen(context.Background(), tenant, now)
	if err != nil {
		t.Fatalf("post-restart is-open: %v", err)
	}
	if !open {
		t.Fatal("breaker state lost across simulated restart — multi-replica safety violated")
	}
}

func TestState_Reset_ClearsBothKeys(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(now)
	s := redisstate.New(srv, "")
	tenant := uuid.New()

	if _, err := s.RecordFailure(context.Background(), tenant, "h", now, time.Hour); err != nil {
		t.Fatalf("seed fail: %v", err)
	}
	if err := s.Trip(context.Background(), tenant, now, time.Hour); err != nil {
		t.Fatalf("trip: %v", err)
	}
	if err := s.Reset(context.Background(), tenant); err != nil {
		t.Fatalf("reset: %v", err)
	}
	open, _ := s.IsOpen(context.Background(), tenant, now)
	if open {
		t.Fatal("breaker still open after Reset")
	}
	got, err := s.RecordFailure(context.Background(), tenant, "h", now, time.Hour)
	if err != nil {
		t.Fatalf("post-reset record: %v", err)
	}
	if got != 1 {
		t.Fatalf("post-reset count = %d, want 1 (failure log should be cleared)", got)
	}
}

func TestState_Reset_IsIdempotentOnUnknownTenant(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(now)
	s := redisstate.New(srv, "")
	if err := s.Reset(context.Background(), uuid.New()); err != nil {
		t.Fatalf("reset on unknown tenant: %v", err)
	}
}

func TestState_Trip_IsIdempotent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(now)
	s := redisstate.New(srv, "")
	tenant := uuid.New()

	if err := s.Trip(context.Background(), tenant, now, time.Hour); err != nil {
		t.Fatalf("first trip: %v", err)
	}
	if err := s.Trip(context.Background(), tenant, now, time.Hour); err != nil {
		t.Fatalf("second trip: %v", err)
	}
	open, _ := s.IsOpen(context.Background(), tenant, now)
	if !open {
		t.Fatal("breaker not open after two trips")
	}
}

func TestState_TenantIsolation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(now)
	s := redisstate.New(srv, "")
	a, b := uuid.New(), uuid.New()

	if err := s.Trip(context.Background(), a, now, time.Hour); err != nil {
		t.Fatalf("trip a: %v", err)
	}
	openA, _ := s.IsOpen(context.Background(), a, now)
	openB, _ := s.IsOpen(context.Background(), b, now)
	if !openA || openB {
		t.Fatalf("isolation broken: a-open=%v b-open=%v (want true/false)", openA, openB)
	}
}

func TestState_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(time.Now())
	s := redisstate.New(srv, "")
	if _, err := s.RecordFailure(context.Background(), uuid.Nil, "h", time.Now(), time.Hour); err == nil {
		t.Fatal("RecordFailure: expected error for uuid.Nil")
	}
	if err := s.Trip(context.Background(), uuid.Nil, time.Now(), time.Hour); err == nil {
		t.Fatal("Trip: expected error for uuid.Nil")
	}
	if _, err := s.IsOpen(context.Background(), uuid.Nil, time.Now()); err == nil {
		t.Fatal("IsOpen: expected error for uuid.Nil")
	}
	if err := s.Reset(context.Background(), uuid.Nil); err == nil {
		t.Fatal("Reset: expected error for uuid.Nil")
	}
}

func TestState_RecordFailure_RejectsZeroWindow(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(time.Now())
	s := redisstate.New(srv, "")
	if _, err := s.RecordFailure(context.Background(), uuid.New(), "h", time.Now(), 0); err == nil {
		t.Fatal("expected error for zero window")
	}
}

func TestState_Trip_RejectsZeroFreeze(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(time.Now())
	s := redisstate.New(srv, "")
	if err := s.Trip(context.Background(), uuid.New(), time.Now(), 0); err == nil {
		t.Fatal("expected error for zero freezeFor")
	}
}

func TestState_RecordFailure_RngError(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(time.Now())
	s := redisstate.New(srv, "")
	s.SetMember(func() (string, error) { return "", errors.New("entropy: drained") })
	if _, err := s.RecordFailure(context.Background(), uuid.New(), "h", time.Now(), time.Hour); err == nil {
		t.Fatal("expected error when rng fails")
	}
}

func TestState_RecordFailure_EvalError(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(time.Now())
	srv.failNext = errors.New("redis: connection refused")
	s := redisstate.New(srv, "")
	if _, err := s.RecordFailure(context.Background(), uuid.New(), "h", time.Now(), time.Hour); err == nil {
		t.Fatal("expected error when redis fails")
	}
}

func TestState_Trip_EvalError(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(time.Now())
	srv.failNext = errors.New("redis: connection refused")
	s := redisstate.New(srv, "")
	if err := s.Trip(context.Background(), uuid.New(), time.Now(), time.Hour); err == nil {
		t.Fatal("expected error when redis fails")
	}
}

func TestState_IsOpen_EvalError(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(time.Now())
	srv.failNext = errors.New("redis: connection refused")
	s := redisstate.New(srv, "")
	if _, err := s.IsOpen(context.Background(), uuid.New(), time.Now()); err == nil {
		t.Fatal("expected error when redis fails")
	}
}

func TestState_Reset_EvalError(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(time.Now())
	srv.failNext = errors.New("redis: connection refused")
	s := redisstate.New(srv, "")
	if err := s.Reset(context.Background(), uuid.New()); err == nil {
		t.Fatal("expected error when redis fails")
	}
}

// stringEvalServer covers the asInt default branch on RecordFailure.
type stringEvalServer struct{}

func (stringEvalServer) Eval(_ context.Context, _ string, _ []string, _ ...any) *goredis.Cmd {
	cmd := &goredis.Cmd{}
	cmd.SetVal("not-a-number")
	return cmd
}

func TestState_RecordFailure_UnexpectedReturnType(t *testing.T) {
	t.Parallel()
	s := redisstate.New(stringEvalServer{}, "")
	if _, err := s.RecordFailure(context.Background(), uuid.New(), "h", time.Now(), time.Hour); err == nil {
		t.Fatal("expected error for non-numeric eval return")
	}
}

func TestState_IsOpen_UnexpectedReturnType(t *testing.T) {
	t.Parallel()
	s := redisstate.New(stringEvalServer{}, "")
	if _, err := s.IsOpen(context.Background(), uuid.New(), time.Now()); err == nil {
		t.Fatal("expected error for non-numeric eval return")
	}
}

// intEvalServer returns int (not int64) to cover the int branch of asInt.
type intEvalServer struct{}

func (intEvalServer) Eval(_ context.Context, _ string, _ []string, _ ...any) *goredis.Cmd {
	cmd := &goredis.Cmd{}
	cmd.SetVal(3)
	return cmd
}

func TestState_RecordFailure_IntReturnType(t *testing.T) {
	t.Parallel()
	s := redisstate.New(intEvalServer{}, "")
	got, err := s.RecordFailure(context.Background(), uuid.New(), "h", time.Now(), time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 3 {
		t.Fatalf("count = %d, want 3", got)
	}
}

func TestState_BreakerEndToEndViaUseCase(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(now)
	state := redisstate.New(srv, "")
	uc := circuitbreaker.New(state, nil, func() time.Time { return now }, circuitbreaker.DefaultConfig())
	tenant := uuid.New()

	for i := 1; i < 5; i++ {
		tripped, err := uc.RecordFailure(context.Background(), tenant, "shop.example.com")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if tripped {
			t.Fatalf("breaker tripped at failure %d; threshold is 5", i)
		}
	}
	tripped, err := uc.RecordFailure(context.Background(), tenant, "shop.example.com")
	if err != nil {
		t.Fatalf("5th failure: %v", err)
	}
	if !tripped {
		t.Fatal("breaker did NOT trip at 5th failure (Redis-backed end-to-end)")
	}
	open, err := uc.IsOpen(context.Background(), tenant, now)
	if err != nil {
		t.Fatalf("IsOpen: %v", err)
	}
	if !open {
		t.Fatal("breaker not open after trip via Redis state")
	}
	if err := uc.RecordSuccess(context.Background(), tenant); err != nil {
		t.Fatalf("RecordSuccess: %v", err)
	}
	open, _ = uc.IsOpen(context.Background(), tenant, now)
	if open {
		t.Fatal("breaker still open after RecordSuccess")
	}
}

func TestState_KeyPrefixDefault(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(now)
	s := redisstate.New(srv, "")
	tenant := uuid.New()
	if err := s.Trip(context.Background(), tenant, now, time.Hour); err != nil {
		t.Fatalf("trip: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	want := "customdomain:lebreaker:frozen:" + tenant.String()
	if _, ok := srv.strings[want]; !ok {
		keys := make([]string, 0, len(srv.strings))
		for k := range srv.strings {
			keys = append(keys, k)
		}
		t.Fatalf("default frozen-key %q not present, keys=%v", want, keys)
	}
}

// portAssertion guarantees redisstate.State satisfies circuitbreaker.State
// at compile time.
func TestState_PortAssertion(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(time.Now())
	var _ circuitbreaker.State = redisstate.New(srv, "")
}
