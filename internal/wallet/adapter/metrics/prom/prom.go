// Package prom is the Prometheus metrics adapter for the wallet.
// It exposes the four metrics required by SIN-62240 AC #6:
//
//   - wallet_commit_retry_total{outcome}
//   - wallet_pending_entries
//   - wallet_reconciliation_drift_pct{wallet_id}
//   - wallet_openrouter_drift_pct{master_id}
package prom

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pericles-luz/crm/internal/wallet/port"
)

// Metrics is the production Prometheus adapter.
type Metrics struct {
	commitRetry            *prometheus.CounterVec
	pendingEntries         prometheus.Gauge
	reconciliationDriftPct *prometheus.GaugeVec
	openrouterDriftPct     *prometheus.GaugeVec
}

// New registers the four metrics on r and returns a Metrics adapter.
// Pass prometheus.DefaultRegisterer to wire into the default Prom HTTP
// handler.
func New(r prometheus.Registerer) *Metrics {
	m := &Metrics{
		commitRetry: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wallet_commit_retry_total",
			Help: "Wallet commit attempts and their outcome (success/retry/enqueued/exhausted).",
		}, []string{"outcome"}),
		pendingEntries: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "wallet_pending_entries",
			Help: "Number of pending ledger entries currently awaiting commit.",
		}),
		reconciliationDriftPct: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "wallet_reconciliation_drift_pct",
			Help: "Drift between Σ posted ledger entries and wallet.balance_movement, as a fraction (1.0 = 100%).",
		}, []string{"wallet_id"}),
		openrouterDriftPct: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "wallet_openrouter_drift_pct",
			Help: "Drift between OpenRouter cost API daily usage and the ledger debit total, as a fraction.",
		}, []string{"master_id"}),
	}
	if r != nil {
		r.MustRegister(m.commitRetry, m.pendingEntries, m.reconciliationDriftPct, m.openrouterDriftPct)
	}
	return m
}

func (m *Metrics) IncCommitRetry(o port.CommitOutcome) {
	m.commitRetry.WithLabelValues(string(o)).Inc()
}

func (m *Metrics) SetPendingEntries(n int64) {
	m.pendingEntries.Set(float64(n))
}

func (m *Metrics) SetReconciliationDriftPct(walletID string, pct float64) {
	m.reconciliationDriftPct.WithLabelValues(walletID).Set(pct)
}

func (m *Metrics) SetOpenRouterDriftPct(masterID string, pct float64) {
	m.openrouterDriftPct.WithLabelValues(masterID).Set(pct)
}
