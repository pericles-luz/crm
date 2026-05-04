// Package memory is an in-process implementation of ratelimit.Limiter.
//
// It exists so that domain tests (use cases, HTTP middleware) can exercise
// the rate-limit code path without a Redis dependency. The algorithm is the
// sliding-window counter described in ADR 0081 §2 — the bucket counter is
// incremented and naturally decays as time passes outside the window.
//
// The implementation is intentionally small (no sub-bucket subdivision)
// because regression tests only need correct deny/allow semantics at the
// window boundaries; the production adapter (internal/ratelimit/adapter/redis)
// is what owns the high-cardinality, distributed implementation.
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/pericles-luz/crm/internal/ratelimit"
)

// Limiter is a goroutine-safe in-memory Limiter. It is suitable for unit
// tests and single-process developer environments; do not use in production
// (counters do not survive restart and are not shared across replicas).
type Limiter struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets map[string]*window
}

// window holds the sample timestamps for one bucket key. We store a slice
// rather than a counter because the test suite asserts behaviour right at
// the window boundary; storing timestamps lets us drop expired samples
// deterministically without depending on a tick goroutine.
type window struct {
	samples []time.Time
}

// Option mutates a Limiter at construction time.
type Option func(*Limiter)

// WithClock overrides the clock the limiter uses to evaluate windows. Tests
// pass a controllable clock so they can assert window boundaries exactly.
func WithClock(now func() time.Time) Option {
	return func(l *Limiter) {
		if now != nil {
			l.now = now
		}
	}
}

// New returns a memory limiter ready for use. By default it uses time.Now.
func New(opts ...Option) *Limiter {
	l := &Limiter{
		now:     time.Now,
		buckets: make(map[string]*window),
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Check implements ratelimit.Limiter.
//
// It honours ratelimit.ErrInvalidLimit when callers pass a zero Limit. It
// never returns ratelimit.ErrUnavailable — the in-memory backend is always
// available — so middleware exercising fail-open / fail-closed paths must
// inject a deliberately broken limiter (see middleware tests).
func (l *Limiter) Check(_ context.Context, key string, limit ratelimit.Limit) (ratelimit.Decision, error) {
	if limit.IsZero() {
		return ratelimit.Decision{}, ratelimit.ErrInvalidLimit
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	cutoff := now.Add(-limit.Window)

	w, ok := l.buckets[key]
	if !ok {
		w = &window{}
		l.buckets[key] = w
	}

	// Drop any sample older than the rolling window — sliding window in the
	// simplest form: one sample per admitted event.
	pruned := w.samples[:0]
	for _, t := range w.samples {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	w.samples = pruned

	if len(w.samples) >= limit.Max {
		oldest := w.samples[0]
		retry := limit.Window - now.Sub(oldest)
		if retry < 0 {
			retry = 0
		}
		return ratelimit.Decision{
			Allowed:   false,
			Remaining: 0,
			Retry:     retry,
		}, nil
	}

	w.samples = append(w.samples, now)
	remaining := limit.Max - len(w.samples)
	resetIn := limit.Window
	if len(w.samples) > 0 {
		resetIn = limit.Window - now.Sub(w.samples[0])
		if resetIn < 0 {
			resetIn = 0
		}
	}
	return ratelimit.Decision{
		Allowed:   true,
		Remaining: remaining,
		Retry:     resetIn,
	}, nil
}

// Reset wipes all counters. Tests use it between cases when they keep the
// same Limiter to amortise allocations.
func (l *Limiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buckets = make(map[string]*window)
}

// Compile-time guard that *Limiter implements ratelimit.Limiter.
var _ ratelimit.Limiter = (*Limiter)(nil)
