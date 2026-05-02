package usecase_test

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pericles-luz/crm/internal/wallet/port"
)

// fakeClock is the deterministic clock used by every use-case test.
// time.Sleep does not actually block — it advances the fake clock and
// records the slept durations for assertions.
type fakeClock struct {
	mu        sync.Mutex
	now       time.Time
	sleeps    []time.Duration
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.sleeps))
	copy(out, c.sleeps)
	return out
}

// seqIDs is a deterministic IDGenerator for tests.
type seqIDs struct{ n atomic.Int64 }

func (s *seqIDs) NewID() string { return "uc-" + itoa(s.n.Add(1)) }

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

// recAlerter records every alert the reconciliator/worker fires.
type recAlerter struct {
	mu     sync.Mutex
	alerts []port.Alert
}

func (r *recAlerter) Send(_ context.Context, a port.Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerts = append(r.alerts, a)
	return nil
}

func (r *recAlerter) Snapshot() []port.Alert {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]port.Alert, len(r.alerts))
	copy(out, r.alerts)
	return out
}

func (r *recAlerter) HasCode(code string) bool {
	for _, a := range r.Snapshot() {
		if a.Code == code {
			return true
		}
	}
	return false
}
