package redis

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	aiport "github.com/pericles-luz/crm/internal/ai/port"
)

// newTestLimiter wires the adapter against a miniredis-backed go-redis client
// with a controllable clock. Returns the limiter, the miniredis instance (so
// tests can inspect Redis state), and a *time.Time the caller advances to
// drive token refill.
func newTestLimiter(t *testing.T) (*Limiter, *miniredis.Miniredis, *time.Time) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := &now
	lim := newWithScripter(rdb, func() time.Time { return *clock })
	return lim, mr, clock
}

func TestAllow_FirstRequest_Allowed(t *testing.T) {
	t.Parallel()
	lim, _, _ := newTestLimiter(t)

	allowed, retry, err := lim.Allow(context.Background(), BucketUserConv, "tenant:t1:user:u1:conv:c1")
	if err != nil {
		t.Fatalf("Allow err = %v", err)
	}
	if !allowed {
		t.Fatalf("first request should be allowed")
	}
	if retry != 0 {
		t.Fatalf("retryAfter on allowed = %v, want 0", retry)
	}
}

func TestAllow_UserConv_OneRequestPer30s(t *testing.T) {
	t.Parallel()
	lim, _, clock := newTestLimiter(t)
	ctx := context.Background()
	key := "tenant:t1:user:u1:conv:c1"

	// First request consumes the only token.
	if allowed, _, err := lim.Allow(ctx, BucketUserConv, key); err != nil || !allowed {
		t.Fatalf("first Allow allowed=%v err=%v", allowed, err)
	}

	// Second request immediately after — denied, retry ~30s.
	allowed, retry, err := lim.Allow(ctx, BucketUserConv, key)
	if err != nil {
		t.Fatalf("second Allow err = %v", err)
	}
	if allowed {
		t.Fatalf("second Allow within 30s should be denied")
	}
	if retry < 29*time.Second || retry > 30*time.Second {
		t.Fatalf("second Allow retryAfter = %v, want ~30s", retry)
	}

	// Advance 29s — still denied.
	*clock = clock.Add(29 * time.Second)
	if allowed, _, _ := lim.Allow(ctx, BucketUserConv, key); allowed {
		t.Fatalf("Allow at +29s should still be denied")
	}

	// Advance to +30s total — allowed again.
	*clock = clock.Add(1100 * time.Millisecond)
	if allowed, _, err := lim.Allow(ctx, BucketUserConv, key); err != nil || !allowed {
		t.Fatalf("Allow at +30s allowed=%v err=%v", allowed, err)
	}
}

func TestAllow_User_TenRequestsPer60s(t *testing.T) {
	t.Parallel()
	lim, _, clock := newTestLimiter(t)
	ctx := context.Background()
	key := "tenant:t1:user:u1"

	// 10 requests in quick succession all allowed.
	for i := 0; i < 10; i++ {
		allowed, _, err := lim.Allow(ctx, BucketUser, key)
		if err != nil {
			t.Fatalf("Allow #%d err = %v", i+1, err)
		}
		if !allowed {
			t.Fatalf("Allow #%d denied, want allowed (capacity 10)", i+1)
		}
	}

	// 11th — denied with retryAfter ~6s (1 / (10/60s) = 6s).
	allowed, retry, err := lim.Allow(ctx, BucketUser, key)
	if err != nil {
		t.Fatalf("Allow #11 err = %v", err)
	}
	if allowed {
		t.Fatalf("Allow #11 should be denied")
	}
	if retry < 5*time.Second || retry > 7*time.Second {
		t.Fatalf("Allow #11 retryAfter = %v, want ~6s", retry)
	}

	// Wait 6.1s — one token refilled, allowed again.
	*clock = clock.Add(6100 * time.Millisecond)
	if allowed, _, err := lim.Allow(ctx, BucketUser, key); err != nil || !allowed {
		t.Fatalf("Allow after 6s refill allowed=%v err=%v", allowed, err)
	}
}

func TestAllow_DifferentKeys_Isolated(t *testing.T) {
	t.Parallel()
	lim, _, _ := newTestLimiter(t)
	ctx := context.Background()

	if allowed, _, err := lim.Allow(ctx, BucketUserConv, "tenant:a:user:u:conv:1"); err != nil || !allowed {
		t.Fatalf("conv 1 first Allow allowed=%v err=%v", allowed, err)
	}
	// Different conv id under the same user — independent bucket.
	if allowed, _, err := lim.Allow(ctx, BucketUserConv, "tenant:a:user:u:conv:2"); err != nil || !allowed {
		t.Fatalf("conv 2 first Allow allowed=%v err=%v", allowed, err)
	}
	// And different tenant under same user — also independent.
	if allowed, _, err := lim.Allow(ctx, BucketUserConv, "tenant:b:user:u:conv:1"); err != nil || !allowed {
		t.Fatalf("tenant b conv 1 first Allow allowed=%v err=%v", allowed, err)
	}
}

func TestAllow_UnknownBucket_FailsClosed(t *testing.T) {
	t.Parallel()
	lim, _, _ := newTestLimiter(t)

	allowed, retry, err := lim.Allow(context.Background(), "ai:panel:bogus", "tenant:t:user:u")
	if !errors.Is(err, aiport.ErrLimiterUnavailable) {
		t.Fatalf("err = %v, want wrapping ErrLimiterUnavailable", err)
	}
	if allowed {
		t.Fatalf("unknown bucket should fail closed (allowed=true)")
	}
	if retry < time.Second {
		t.Fatalf("retryAfter on fail-closed = %v, want >= 1s", retry)
	}
}

func TestAllow_EmptyKey_FailsClosed(t *testing.T) {
	t.Parallel()
	lim, _, _ := newTestLimiter(t)

	allowed, retry, err := lim.Allow(context.Background(), BucketUserConv, "")
	if !errors.Is(err, aiport.ErrLimiterUnavailable) {
		t.Fatalf("err = %v, want wrapping ErrLimiterUnavailable", err)
	}
	if allowed || retry == 0 {
		t.Fatalf("empty key should fail closed with non-zero retryAfter; got allowed=%v retry=%v", allowed, retry)
	}
}

func TestAllow_RedisDown_FailsClosed(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	lim := newWithScripter(rdb, time.Now)

	mr.Close() // simulate Redis going down

	allowed, retry, err := lim.Allow(context.Background(), BucketUser, "tenant:t:user:u")
	if !errors.Is(err, aiport.ErrLimiterUnavailable) {
		t.Fatalf("err = %v, want wrapping ErrLimiterUnavailable", err)
	}
	if allowed {
		t.Fatalf("Redis down should fail closed")
	}
	if retry < time.Second {
		t.Fatalf("retryAfter when Redis down = %v, want >= 1s", retry)
	}
}

func TestNewLimiter_ReturnsPortInterface(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	var lim aiport.RateLimiter = NewLimiter(rdb)
	if _, _, err := lim.Allow(context.Background(), BucketUser, "tenant:t:user:u"); err != nil {
		t.Fatalf("Allow err = %v", err)
	}
}

func TestParseScriptResult_BadShape(t *testing.T) {
	t.Parallel()
	cases := []any{
		nil,
		"not-an-array",
		[]any{int64(1)},
		[]any{int64(1), int64(0), int64(0)},
		[]any{[]byte{0x01}, int64(0)},
		[]any{int64(1), make(chan int)},
	}
	for i, c := range cases {
		c := c
		t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
			t.Parallel()
			if _, _, err := parseScriptResult(c); err == nil {
				t.Fatalf("parseScriptResult(%v) err = nil, want error", c)
			}
		})
	}
}

func TestParseScriptResult_NegativeRetryClamped(t *testing.T) {
	t.Parallel()
	allowed, retry, err := parseScriptResult([]any{int64(1), int64(-50)})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !allowed {
		t.Fatalf("allowed = false, want true")
	}
	if retry != 0 {
		t.Fatalf("retry = %v, want 0 (negative clamped)", retry)
	}
}

func TestToInt64_AllSupportedTypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   any
		want int64
	}{
		{int64(7), 7},
		{int(8), 8},
		{float64(9), 9},
		{"10", 10},
	}
	for _, c := range cases {
		got, err := toInt64(c.in)
		if err != nil {
			t.Fatalf("toInt64(%v) err = %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("toInt64(%v) = %d, want %d", c.in, got, c.want)
		}
	}
	if _, err := toInt64(struct{}{}); err == nil {
		t.Fatalf("toInt64(struct{}{}) err = nil, want error")
	}
	if _, err := toInt64("notanumber"); err == nil {
		t.Fatalf("toInt64(\"notanumber\") err = nil, want error")
	}
}
