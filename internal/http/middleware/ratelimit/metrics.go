// Package ratelimit provides the AI panel rate-limit HTTP middleware
// (SIN-62238) and the Prometheus telemetry it emits. The middleware
// stacks two token-bucket checks (per-user-conv and per-user) supplied
// by an injected port.RateLimiter; on rejection it answers 429 (or 503
// when the limiter backend is unavailable) and emits the
// ai_panel_rate_limited_total counter so the team can observe spam
// patterns before tuning the policy.
//
// "Observability before optimization" is one of the lenses applied to
// SIN-62225 §3.6 — counters are wired up in the very first iteration so
// we can see the live rate of denials in staging before tightening or
// relaxing the bucket parameters.
package ratelimit

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// rateLimitedTotal counts AI panel requests that the middleware rejected,
// partitioned by which bucket caused the rejection. The reason label
// distinguishes quota denials ("quota") from fail-closed backend errors
// ("backend_unavailable") so SREs can alert on the latter independently.
var (
	rateLimitedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ai_panel_rate_limited_total",
			Help: "Total AI panel requests rejected by the rate limiter, partitioned by bucket and reason.",
		},
		[]string{"bucket", "reason"},
	)

	registerOnce sync.Once
	registerErr  error
)

// MustRegister registers the package's metrics with reg. It is idempotent
// across calls in the same process — subsequent calls are no-ops, which
// makes it safe to call from main and from tests with their own registries.
//
// Returning the error (instead of panicking like prometheus.MustRegister)
// lets callers choose: production main can wrap in log.Fatal, while tests
// can swap in a fresh registry without crashing the suite.
func MustRegister(reg prometheus.Registerer) error {
	registerOnce.Do(func() {
		registerErr = reg.Register(rateLimitedTotal)
	})
	return registerErr
}

// resetMetricsForTest re-creates the counter and resets the once guard so
// that tests can assert exact counter values from a known-zero baseline.
// Test-only — never call from production code.
func resetMetricsForTest() {
	rateLimitedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ai_panel_rate_limited_total",
			Help: "Total AI panel requests rejected by the rate limiter, partitioned by bucket and reason.",
		},
		[]string{"bucket", "reason"},
	)
	registerOnce = sync.Once{}
	registerErr = nil
}
