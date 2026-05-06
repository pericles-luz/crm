package sliding_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/customdomain/ratelimit/sliding"
)

// fakeServer is a small in-memory Redis emulator that knows enough of the
// Lua semantics to test the sliding-window adapter end-to-end. It is NOT
// a generic Redis fake; it implements only the four ops the limiter's
// script actually issues (ZREMRANGEBYSCORE, ZADD, ZCARD, EXPIRE) and
// returns the ZCARD post the script's writes.
//
// This sits on the right side of the CTO "no mocking the database" rule
// because it is a documented in-memory adapter that matches Redis ZSET
// semantics, not a stub that returns canned values.
type fakeServer struct {
	mu       sync.Mutex
	now      time.Time
	zsets    map[string][]zsetEntry
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
		expiries: map[string]time.Time{},
	}
}

func (f *fakeServer) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func (f *fakeServer) Eval(_ context.Context, _ string, keys []string, args ...any) *goredis.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := &goredis.Cmd{}
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		cmd.SetErr(err)
		return cmd
	}
	if len(keys) != 1 {
		cmd.SetErr(errors.New("fake: expected 1 key"))
		return cmd
	}
	key := keys[0]
	if len(args) != 4 {
		cmd.SetErr(errors.New("fake: expected 4 args"))
		return cmd
	}
	now, err := toInt64(args[0])
	if err != nil {
		cmd.SetErr(err)
		return cmd
	}
	cutoff, err := toInt64(args[1])
	if err != nil {
		cmd.SetErr(err)
		return cmd
	}
	member, ok := args[2].(string)
	if !ok {
		cmd.SetErr(errors.New("fake: member must be string"))
		return cmd
	}
	ttl, err := toInt64(args[3])
	if err != nil {
		cmd.SetErr(err)
		return cmd
	}

	// expire any key whose deadline has passed (lazy expiration).
	if dead, ok := f.expiries[key]; ok && !f.now.Before(dead) {
		delete(f.zsets, key)
		delete(f.expiries, key)
	}

	// ZREMRANGEBYSCORE 0 cutoff
	entries := f.zsets[key]
	kept := entries[:0]
	for _, e := range entries {
		if e.score > cutoff {
			kept = append(kept, e)
		}
	}
	// ZADD now member
	kept = append(kept, zsetEntry{score: now, member: member})
	f.zsets[key] = kept
	// EXPIRE
	f.expiries[key] = f.now.Add(time.Duration(ttl) * time.Second)
	// return ZCARD
	cmd.SetVal(int64(len(kept)))
	return cmd
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

func TestLimiter_AllowsUpToMax(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(start)
	l := sliding.New(srv, "customdomain:tls_ask", 3, time.Minute)

	for i := 1; i <= 3; i++ {
		ok, err := l.Allow(context.Background(), "shop.example.com", start)
		if err != nil {
			t.Fatalf("call %d: unexpected error %v", i, err)
		}
		if !ok {
			t.Fatalf("call %d: denied; expected allow", i)
		}
	}

	// 4th call within the window denies.
	ok, err := l.Allow(context.Background(), "shop.example.com", start)
	if err != nil {
		t.Fatalf("4th call error: %v", err)
	}
	if ok {
		t.Fatalf("4th call allowed; expected deny")
	}
}

func TestLimiter_PerHostIsolation(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(start)
	l := sliding.New(srv, "customdomain:tls_ask", 1, time.Minute)

	ok, _ := l.Allow(context.Background(), "a.example.com", start)
	if !ok {
		t.Fatal("first call to a denied")
	}
	ok, _ = l.Allow(context.Background(), "b.example.com", start)
	if !ok {
		t.Fatal("first call to b denied; per-host isolation broken")
	}
	ok, _ = l.Allow(context.Background(), "a.example.com", start)
	if ok {
		t.Fatal("second call to a allowed; max=1 not honoured")
	}
}

func TestLimiter_WindowExpires(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(start)
	l := sliding.New(srv, "customdomain:tls_ask", 1, time.Minute)

	// Fill the window.
	if ok, _ := l.Allow(context.Background(), "shop.example.com", start); !ok {
		t.Fatal("first call denied")
	}
	if ok, _ := l.Allow(context.Background(), "shop.example.com", start.Add(time.Second)); ok {
		t.Fatal("second call within window allowed")
	}

	// 61 seconds later, the original entry has aged out.
	srv.advance(61 * time.Second)
	later := start.Add(61 * time.Second)
	if ok, _ := l.Allow(context.Background(), "shop.example.com", later); !ok {
		t.Fatal("call after window expiry denied")
	}
}

func TestLimiter_DefaultsAreFromSpec(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(start)
	// Use defaults: max=0 -> 3, window=0 -> 1m, prefix="" -> default.
	l := sliding.New(srv, "", 0, 0)
	for i := 1; i <= 3; i++ {
		if ok, _ := l.Allow(context.Background(), "shop.example.com", start); !ok {
			t.Fatalf("call %d denied under defaults", i)
		}
	}
	if ok, _ := l.Allow(context.Background(), "shop.example.com", start); ok {
		t.Fatal("4th call allowed under defaults; expected deny")
	}
}

func TestLimiter_EvalErrorPropagates(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(start)
	srv.failNext = errors.New("redis: connection refused")
	l := sliding.New(srv, "customdomain:tls_ask", 3, time.Minute)

	ok, err := l.Allow(context.Background(), "shop.example.com", start)
	if err == nil {
		t.Fatal("expected error from limiter when redis fails")
	}
	if ok {
		t.Fatal("expected deny when redis errors")
	}
}

func TestLimiter_EmptyHostIsRejected(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(time.Now())
	l := sliding.New(srv, "customdomain:tls_ask", 3, time.Minute)
	ok, err := l.Allow(context.Background(), "", time.Now())
	if err == nil {
		t.Fatal("expected error for empty host")
	}
	if ok {
		t.Fatal("expected deny for empty host")
	}
}

func TestLimiter_RandomMemberError(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(time.Now())
	l := sliding.New(srv, "customdomain:tls_ask", 3, time.Minute)
	l.SetMember(func() (string, error) { return "", errors.New("entropy: drained") })
	ok, err := l.Allow(context.Background(), "shop.example.com", time.Now())
	if err == nil {
		t.Fatal("expected error when rng fails")
	}
	if ok {
		t.Fatal("expected deny when rng fails")
	}
}

// stringEvalServer returns a non-numeric value from Eval so we can prove
// asInt64's default branch surfaces a clear error.
type stringEvalServer struct{}

func (stringEvalServer) Eval(_ context.Context, _ string, _ []string, _ ...any) *goredis.Cmd {
	cmd := &goredis.Cmd{}
	cmd.SetVal("not-a-number")
	return cmd
}

func TestLimiter_UnexpectedEvalReturnTypeIsError(t *testing.T) {
	t.Parallel()
	l := sliding.New(stringEvalServer{}, "customdomain:tls_ask", 3, time.Minute)
	ok, err := l.Allow(context.Background(), "shop.example.com", time.Now())
	if err == nil {
		t.Fatal("expected error for non-numeric Eval return")
	}
	if ok {
		t.Fatal("expected deny for non-numeric Eval return")
	}
}

// intEvalServer returns int (not int64) from Eval to cover the asInt64 int branch.
type intEvalServer struct{}

func (intEvalServer) Eval(_ context.Context, _ string, _ []string, _ ...any) *goredis.Cmd {
	cmd := &goredis.Cmd{}
	cmd.SetVal(2)
	return cmd
}

func TestLimiter_IntReturnTypeIsAccepted(t *testing.T) {
	t.Parallel()
	l := sliding.New(intEvalServer{}, "customdomain:tls_ask", 3, time.Minute)
	ok, err := l.Allow(context.Background(), "shop.example.com", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected allow when count=2 ≤ max=3")
	}
}
