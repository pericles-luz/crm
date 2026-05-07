package rediswindow_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/customdomain/enrollment"
	"github.com/pericles-luz/crm/internal/customdomain/enrollment/rediswindow"
)

// fakeServer is the same pattern as internal/customdomain/ratelimit/sliding's
// fake — an in-memory adapter that matches Redis ZSET semantics for the four
// ops the Lua script issues (ZREMRANGEBYSCORE, ZADD, ZCARD, EXPIRE). This
// keeps the unit test on the right side of the CTO "no mocking the
// database" rule: a documented in-memory adapter that matches Redis
// behaviour, not a stub returning canned values.
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
	score, err := toInt64(args[0])
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

	if dead, ok := f.expiries[key]; ok && !f.now.Before(dead) {
		delete(f.zsets, key)
		delete(f.expiries, key)
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

func TestCounter_CountAndRecord_HourQuotaBoundary(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(start)
	c := rediswindow.New(srv, "")
	tenant := uuid.New()

	for i := 1; i <= 5; i++ {
		got, err := c.CountAndRecord(context.Background(), tenant, enrollment.WindowHour, start)
		if err != nil {
			t.Fatalf("call %d: unexpected error %v", i, err)
		}
		if got != i {
			t.Fatalf("call %d: count = %d, want %d", i, got, i)
		}
	}
	got, err := c.CountAndRecord(context.Background(), tenant, enrollment.WindowHour, start)
	if err != nil {
		t.Fatalf("call 6: unexpected error %v", err)
	}
	if got != 6 {
		t.Fatalf("call 6: count = %d, want 6", got)
	}
}

func TestCounter_CountAndRecord_PerTenantIsolation(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(start)
	c := rediswindow.New(srv, "")
	a, b := uuid.New(), uuid.New()

	for i := 1; i <= 3; i++ {
		if _, err := c.CountAndRecord(context.Background(), a, enrollment.WindowHour, start); err != nil {
			t.Fatalf("a call %d: %v", i, err)
		}
	}
	got, err := c.CountAndRecord(context.Background(), b, enrollment.WindowHour, start)
	if err != nil {
		t.Fatalf("b call: %v", err)
	}
	if got != 1 {
		t.Fatalf("tenant b leaked tenant a's count: got %d, want 1", got)
	}
}

func TestCounter_CountAndRecord_PerWindowIsolation(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(start)
	c := rediswindow.New(srv, "")
	tenant := uuid.New()

	if _, err := c.CountAndRecord(context.Background(), tenant, enrollment.WindowHour, start); err != nil {
		t.Fatalf("hour: %v", err)
	}
	got, err := c.CountAndRecord(context.Background(), tenant, enrollment.WindowDay, start)
	if err != nil {
		t.Fatalf("day: %v", err)
	}
	if got != 1 {
		t.Fatalf("day count leaked from hour: got %d, want 1", got)
	}
	got, err = c.CountAndRecord(context.Background(), tenant, enrollment.WindowMonth, start)
	if err != nil {
		t.Fatalf("month: %v", err)
	}
	if got != 1 {
		t.Fatalf("month count leaked: got %d, want 1", got)
	}
}

func TestCounter_CountAndRecord_HourWindowExpires(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	srv := newFakeServer(start)
	c := rediswindow.New(srv, "")
	tenant := uuid.New()

	for i := 1; i <= 3; i++ {
		if _, err := c.CountAndRecord(context.Background(), tenant, enrollment.WindowHour, start); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// 1h + 1s later, all three previous entries have aged out.
	srv.advance(time.Hour + time.Second)
	later := start.Add(time.Hour + time.Second)
	got, err := c.CountAndRecord(context.Background(), tenant, enrollment.WindowHour, later)
	if err != nil {
		t.Fatalf("post-expiry: %v", err)
	}
	if got != 1 {
		t.Fatalf("post-expiry count = %d, want 1 (only the new call should be in the window)", got)
	}
}

func TestCounter_CountAndRecord_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(time.Now())
	c := rediswindow.New(srv, "")
	_, err := c.CountAndRecord(context.Background(), uuid.Nil, enrollment.WindowHour, time.Now())
	if err == nil {
		t.Fatal("expected error for uuid.Nil tenant")
	}
}

func TestCounter_CountAndRecord_RngError(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(time.Now())
	c := rediswindow.New(srv, "")
	c.SetMember(func() (string, error) { return "", errors.New("entropy: drained") })
	_, err := c.CountAndRecord(context.Background(), uuid.New(), enrollment.WindowHour, time.Now())
	if err == nil {
		t.Fatal("expected error when rng fails")
	}
}

func TestCounter_CountAndRecord_EvalError(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(time.Now())
	srv.failNext = errors.New("redis: connection refused")
	c := rediswindow.New(srv, "")
	_, err := c.CountAndRecord(context.Background(), uuid.New(), enrollment.WindowHour, time.Now())
	if err == nil {
		t.Fatal("expected error when redis eval fails")
	}
}

// stringEvalServer returns a non-numeric value so the asInt default branch
// surfaces a clear error.
type stringEvalServer struct{}

func (stringEvalServer) Eval(_ context.Context, _ string, _ []string, _ ...any) *goredis.Cmd {
	cmd := &goredis.Cmd{}
	cmd.SetVal("not-a-number")
	return cmd
}

func TestCounter_UnexpectedEvalReturnTypeIsError(t *testing.T) {
	t.Parallel()
	c := rediswindow.New(stringEvalServer{}, "")
	_, err := c.CountAndRecord(context.Background(), uuid.New(), enrollment.WindowHour, time.Now())
	if err == nil {
		t.Fatal("expected error for non-numeric Eval return")
	}
}

// intEvalServer returns int (not int64) to cover the int branch of asInt.
type intEvalServer struct{}

func (intEvalServer) Eval(_ context.Context, _ string, _ []string, _ ...any) *goredis.Cmd {
	cmd := &goredis.Cmd{}
	cmd.SetVal(2)
	return cmd
}

func TestCounter_IntReturnTypeIsAccepted(t *testing.T) {
	t.Parallel()
	c := rediswindow.New(intEvalServer{}, "")
	got, err := c.CountAndRecord(context.Background(), uuid.New(), enrollment.WindowHour, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
}

func TestCounter_KeyPrefix_DefaultsToCustomdomainEnrollment(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(time.Now())
	c := rediswindow.New(srv, "")
	tenant := uuid.New()
	if _, err := c.CountAndRecord(context.Background(), tenant, enrollment.WindowHour, time.Now()); err != nil {
		t.Fatalf("call: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	wantKey := "customdomain:enrollment:" + tenant.String() + ":hour"
	if _, ok := srv.zsets[wantKey]; !ok {
		t.Fatalf("expected default key %q in zsets, got keys %v", wantKey, mapKeys(srv.zsets))
	}
}

func mapKeys(m map[string][]zsetEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestCounter_PortAssertion(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(time.Now())
	var _ enrollment.WindowCounter = rediswindow.New(srv, "")
}
