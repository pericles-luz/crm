// Package prom is the Prometheus metrics adapter for the wallet.
// It exposes the four metrics required by SIN-62240 AC #6:
//
//   - wallet_commit_retry_total{outcome}
//   - wallet_pending_entries
//   - wallet_reconciliation_drift_pct{wallet_id}
//   - wallet_openrouter_drift_pct{master_id}
package prom

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/pericles-luz/crm/internal/wallet/port"
)

// Metrics is the production Prometheus adapter.
//
// seenWalletIDs/seenMasterIDs track every label value the adapter has
// ever observed so RetainReconciliationDriftLabels /
// RetainOpenRouterDriftLabels can call DeleteLabelValues for ids that
// fell out of the latest reconciliator pass — without that, the
// Prometheus registry leaks dead label series proportional to wallet
// churn (SIN-62269).
type Metrics struct {
	commitRetry            *prometheus.CounterVec
	pendingEntries         prometheus.Gauge
	reconciliationDriftPct *prometheus.GaugeVec
	openrouterDriftPct     *prometheus.GaugeVec

	mu            sync.Mutex
	seenWalletIDs map[string]struct{}
	seenMasterIDs map[string]struct{}
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
		seenWalletIDs: map[string]struct{}{},
		seenMasterIDs: map[string]struct{}{},
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
	m.mu.Lock()
	m.seenWalletIDs[walletID] = struct{}{}
	m.mu.Unlock()
	m.reconciliationDriftPct.WithLabelValues(walletID).Set(pct)
}

func (m *Metrics) SetOpenRouterDriftPct(masterID string, pct float64) {
	m.mu.Lock()
	m.seenMasterIDs[masterID] = struct{}{}
	m.mu.Unlock()
	m.openrouterDriftPct.WithLabelValues(masterID).Set(pct)
}

// RetainReconciliationDriftLabels deletes wallet_reconciliation_drift_pct
// label series whose wallet_id is not in active. Callers should pass the
// full set of wallet ids observed in the just-finished reconciliator
// pass; ids previously seen but absent from this set are pruned via
// DeleteLabelValues.
func (m *Metrics) RetainReconciliationDriftLabels(active []string) {
	m.retain(&m.seenWalletIDs, m.reconciliationDriftPct, active)
}

// RetainOpenRouterDriftLabels deletes wallet_openrouter_drift_pct label
// series whose master_id is not in active. Same contract as
// RetainReconciliationDriftLabels but keyed on master id.
func (m *Metrics) RetainOpenRouterDriftLabels(active []string) {
	m.retain(&m.seenMasterIDs, m.openrouterDriftPct, active)
}

func (m *Metrics) retain(seen *map[string]struct{}, vec *prometheus.GaugeVec, active []string) {
	keep := make(map[string]struct{}, len(active))
	for _, id := range active {
		keep[id] = struct{}{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range *seen {
		if _, ok := keep[id]; ok {
			continue
		}
		vec.DeleteLabelValues(id)
		delete(*seen, id)
	}
}
