// Package noop is the metrics adapter used in tests and in environments
// where Prometheus is not wired yet. It satisfies port.Metrics with
// concurrent-safe in-memory counters so tests can assert on them.
package noop

import (
	"sync"

	"github.com/pericles-luz/crm/internal/wallet/port"
)

// Metrics is a recording in-memory metrics adapter.
type Metrics struct {
	mu              sync.Mutex
	commitRetry     map[port.CommitOutcome]int64
	pending         int64
	driftByWallet   map[string]float64
	driftByMaster   map[string]float64
}

// New constructs an empty Metrics.
func New() *Metrics {
	return &Metrics{
		commitRetry:   map[port.CommitOutcome]int64{},
		driftByWallet: map[string]float64{},
		driftByMaster: map[string]float64{},
	}
}

func (m *Metrics) IncCommitRetry(o port.CommitOutcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commitRetry[o]++
}

func (m *Metrics) SetPendingEntries(n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = n
}

func (m *Metrics) SetReconciliationDriftPct(walletID string, pct float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.driftByWallet[walletID] = pct
}

func (m *Metrics) SetOpenRouterDriftPct(masterID string, pct float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.driftByMaster[masterID] = pct
}

// Snapshot returns a copy of every counter and gauge for assertions.
type Snapshot struct {
	CommitRetry      map[port.CommitOutcome]int64
	Pending          int64
	DriftByWallet    map[string]float64
	DriftByMaster    map[string]float64
}

// Snapshot returns a deep copy of internal state.
func (m *Metrics) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	cr := make(map[port.CommitOutcome]int64, len(m.commitRetry))
	for k, v := range m.commitRetry {
		cr[k] = v
	}
	dw := make(map[string]float64, len(m.driftByWallet))
	for k, v := range m.driftByWallet {
		dw[k] = v
	}
	dm := make(map[string]float64, len(m.driftByMaster))
	for k, v := range m.driftByMaster {
		dm[k] = v
	}
	return Snapshot{CommitRetry: cr, Pending: m.pending, DriftByWallet: dw, DriftByMaster: dm}
}
