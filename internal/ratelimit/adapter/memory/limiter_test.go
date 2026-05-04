package memory_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/ratelimit"
	"github.com/pericles-luz/crm/internal/ratelimit/adapter/memory"
)

func TestLimiter_AllowsUntilMax(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(0, 0))
	lim := memory.New(memory.WithClock(clock.Now))

	limit := ratelimit.Limit{Window: time.Minute, Max: 3}
	for i := 1; i <= 3; i++ {
		dec, err := lim.Check(context.Background(), "k", limit)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if !dec.Allowed {
			t.Fatalf("call %d: must be allowed (under Max)", i)
		}
		if want := 3 - i; dec.Remaining != want {
			t.Fatalf("call %d: remaining = %d, want %d", i, dec.Remaining, want)
		}
	}

	dec, err := lim.Check(context.Background(), "k", limit)
	if err != nil {
		t.Fatalf("4th call err: %v", err)
	}
	if dec.Allowed {
		t.Fatal("4th call must be denied")
	}
	if dec.Remaining != 0 {
		t.Fatalf("denied Remaining = %d, want 0", dec.Remaining)
	}
	if dec.Retry <= 0 {
		t.Fatalf("denied Retry = %v, want positive", dec.Retry)
	}
}

func TestLimiter_RecoversAfterWindow(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(0, 0))
	lim := memory.New(memory.WithClock(clock.Now))

	limit := ratelimit.Limit{Window: 10 * time.Second, Max: 2}
	for i := 0; i < 2; i++ {
		if _, err := lim.Check(context.Background(), "k", limit); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	dec, _ := lim.Check(context.Background(), "k", limit)
	if dec.Allowed {
		t.Fatal("3rd call inside window must be denied")
	}

	// Step clock past the rolling window — the counter should drain.
	clock.Advance(11 * time.Second)

	dec, err := lim.Check(context.Background(), "k", limit)
	if err != nil {
		t.Fatalf("post-window err: %v", err)
	}
	if !dec.Allowed {
		t.Fatal("post-window call must be allowed once samples expire")
	}
}

func TestLimiter_KeysAreIndependent(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(0, 0))
	lim := memory.New(memory.WithClock(clock.Now))
	limit := ratelimit.Limit{Window: time.Minute, Max: 1}

	if d, _ := lim.Check(context.Background(), "alice", limit); !d.Allowed {
		t.Fatal("alice first call must be allowed")
	}
	if d, _ := lim.Check(context.Background(), "bob", limit); !d.Allowed {
		t.Fatal("bob first call must not share alice's bucket")
	}
}

func TestLimiter_RejectsZeroLimit(t *testing.T) {
	t.Parallel()
	lim := memory.New()
	_, err := lim.Check(context.Background(), "k", ratelimit.Limit{})
	if !errors.Is(err, ratelimit.ErrInvalidLimit) {
		t.Fatalf("zero Limit error = %v, want ErrInvalidLimit", err)
	}
}

func TestLimiter_RetryHonoursOldestSample(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(0, 0))
	lim := memory.New(memory.WithClock(clock.Now))
	limit := ratelimit.Limit{Window: 60 * time.Second, Max: 1}

	if _, err := lim.Check(context.Background(), "k", limit); err != nil {
		t.Fatalf("seed: %v", err)
	}
	clock.Advance(15 * time.Second)
	dec, err := lim.Check(context.Background(), "k", limit)
	if err != nil {
		t.Fatalf("denied call err: %v", err)
	}
	if dec.Allowed {
		t.Fatal("call within window must be denied")
	}
	wantRetry := 45 * time.Second
	if dec.Retry != wantRetry {
		t.Fatalf("Retry = %v, want %v", dec.Retry, wantRetry)
	}
}

func TestLimiter_Reset(t *testing.T) {
	t.Parallel()
	lim := memory.New()
	limit := ratelimit.Limit{Window: time.Minute, Max: 1}
	if _, err := lim.Check(context.Background(), "k", limit); err != nil {
		t.Fatalf("seed: %v", err)
	}
	lim.Reset()
	dec, err := lim.Check(context.Background(), "k", limit)
	if err != nil {
		t.Fatalf("post-reset err: %v", err)
	}
	if !dec.Allowed {
		t.Fatal("post-reset call must be allowed (counter wiped)")
	}
}

func TestLimiter_Concurrent(t *testing.T) {
	t.Parallel()
	lim := memory.New()
	limit := ratelimit.Limit{Window: time.Minute, Max: 100}

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				dec, err := lim.Check(context.Background(), "shared", limit)
				if err != nil {
					return
				}
				if dec.Allowed {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != int64(limit.Max) {
		t.Fatalf("concurrent allowed = %d, want %d (Max)", got, limit.Max)
	}
}

// fakeClock is a deterministic clock for window-boundary tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
