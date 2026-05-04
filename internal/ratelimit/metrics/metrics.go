// Package metrics declares the rate-limit observability port (ADR 0081 §6).
//
// The HTTP middleware emits one Recorder call per Check outcome. Production
// wires the prom adapter (internal/ratelimit/metrics/prom); tests inject the
// in-memory Counter to assert that allowed/denied/unavailable counters move
// without dragging Prometheus into the unit tests.
package metrics

import "sync"

// Recorder records the three rate-limit outcomes per ADR 0081 §6:
//
//   - Allowed:     ratelimit_allowed_total{endpoint, bucket}
//   - Denied:      ratelimit_denied_total{endpoint, bucket}
//   - Unavailable: ratelimit_unavailable_total{endpoint}
//
// Implementations MUST be safe for concurrent use. Tests should prefer the
// in-memory Counter type below.
type Recorder interface {
	Allowed(endpoint, bucket string)
	Denied(endpoint, bucket string)
	Unavailable(endpoint string)
}

// Noop is a Recorder that drops every event. It is the safe default when
// the wiring code has not yet plugged in a real backend.
type Noop struct{}

// Allowed implements Recorder.
func (Noop) Allowed(string, string) {}

// Denied implements Recorder.
func (Noop) Denied(string, string) {}

// Unavailable implements Recorder.
func (Noop) Unavailable(string) {}

// Counter is an in-memory Recorder for tests. Counts are keyed by a
// "endpoint|bucket" tuple so that assertions can target a specific (endpoint,
// bucket) pair without having to reason about label cardinality.
type Counter struct {
	mu          sync.Mutex
	allowed     map[string]int
	denied      map[string]int
	unavailable map[string]int
}

// NewCounter returns a Counter with empty maps.
func NewCounter() *Counter {
	return &Counter{
		allowed:     make(map[string]int),
		denied:      make(map[string]int),
		unavailable: make(map[string]int),
	}
}

// Allowed implements Recorder.
func (c *Counter) Allowed(endpoint, bucket string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.allowed[key(endpoint, bucket)]++
}

// Denied implements Recorder.
func (c *Counter) Denied(endpoint, bucket string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.denied[key(endpoint, bucket)]++
}

// Unavailable implements Recorder.
func (c *Counter) Unavailable(endpoint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unavailable[endpoint]++
}

// AllowedCount returns the recorded Allowed count for (endpoint, bucket).
func (c *Counter) AllowedCount(endpoint, bucket string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.allowed[key(endpoint, bucket)]
}

// DeniedCount returns the recorded Denied count for (endpoint, bucket).
func (c *Counter) DeniedCount(endpoint, bucket string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.denied[key(endpoint, bucket)]
}

// UnavailableCount returns the recorded Unavailable count for endpoint.
func (c *Counter) UnavailableCount(endpoint string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unavailable[endpoint]
}

func key(endpoint, bucket string) string { return endpoint + "|" + bucket }

// Compile-time guards.
var (
	_ Recorder = Noop{}
	_ Recorder = (*Counter)(nil)
)
