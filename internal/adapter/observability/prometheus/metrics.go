// Package prometheus implements webhook.Metrics on top of the
// prometheus/client_golang library. Counters and the histogram match
// §5 of ADR 0075. tenant_id labels are emitted only when the outcome
// is post-HMAC authenticated; pre-HMAC outcomes go to the unlabelled
// counter so the cardinality stays bounded.
package prometheus

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/pericles-luz/crm/internal/webhook"
)

// Metrics is the webhook.Metrics implementation.
type Metrics struct {
	receivedPre  *prometheus.CounterVec
	receivedAuth *prometheus.CounterVec
	ack          *prometheus.HistogramVec
	idemConflict *prometheus.CounterVec
}

// New constructs a Metrics bound to reg. Returns the value so it can be
// passed by value into webhook.Config; reg.MustRegister panics on
// duplicate registration which is the correct behaviour at startup.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		receivedPre: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "webhook_received_total",
			Help: "Webhook deliveries received, classified by terminal outcome (pre-HMAC paths, no tenant label).",
		}, []string{"channel", "outcome"}),
		receivedAuth: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "webhook_received_authenticated_total",
			Help: "Webhook deliveries received with an authenticated tenant.",
		}, []string{"channel", "outcome", "tenant_id"}),
		ack: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "webhook_ack_duration_seconds",
			Help:    "Time from request entry to 200 OK ack.",
			Buckets: prometheus.DefBuckets,
		}, []string{"channel"}),
		idemConflict: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "webhook_idempotency_conflict_total",
			Help: "Idempotency conflicts (replay) for an authenticated tenant.",
		}, []string{"channel", "tenant_id"}),
	}
	reg.MustRegister(m.receivedPre, m.receivedAuth, m.ack, m.idemConflict)
	return m
}

// IncReceived implements webhook.Metrics. tenant_id label is emitted
// only for authenticated outcomes — see ADR §5.
func (m *Metrics) IncReceived(channel string, outcome webhook.Outcome, tenantID webhook.TenantID, hasTenant bool) {
	if hasTenant && outcome.IsAuthenticated() {
		m.receivedAuth.WithLabelValues(channel, string(outcome), tenantID.String()).Inc()
		return
	}
	m.receivedPre.WithLabelValues(channel, string(outcome)).Inc()
}

// ObserveAck implements webhook.Metrics.
func (m *Metrics) ObserveAck(channel string, d time.Duration) {
	m.ack.WithLabelValues(channel).Observe(d.Seconds())
}

// IncIdempotencyConflict implements webhook.Metrics.
func (m *Metrics) IncIdempotencyConflict(channel string, tenantID webhook.TenantID) {
	m.idemConflict.WithLabelValues(channel, tenantID.String()).Inc()
}
