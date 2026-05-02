package prom_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	prommetrics "github.com/pericles-luz/crm/internal/wallet/adapter/metrics/prom"
	"github.com/pericles-luz/crm/internal/wallet/port"
)

func TestProm_RegistersAndIncrements(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := prommetrics.New(reg)

	m.IncCommitRetry(port.OutcomeSuccess)
	m.IncCommitRetry(port.OutcomeRetry)
	m.IncCommitRetry(port.OutcomeRetry)
	m.SetPendingEntries(3)
	m.SetReconciliationDriftPct("w1", 0.02)
	m.SetOpenRouterDriftPct("m1", 0.07)

	// All four metrics must be registered with their AC-mandated names.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	want := map[string]bool{
		"wallet_commit_retry_total":       false,
		"wallet_pending_entries":          false,
		"wallet_reconciliation_drift_pct": false,
		"wallet_openrouter_drift_pct":     false,
	}
	for _, mf := range mfs {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric %s not registered", name)
		}
	}

	// Sanity: wallet_commit_retry_total must have two outcome series (success + retry).
	out := 0
	for _, mf := range mfs {
		if mf.GetName() == "wallet_commit_retry_total" {
			out = len(mf.GetMetric())
		}
	}
	if out < 2 {
		t.Errorf("counter series: got %d, want >= 2", out)
	}
}

// TestProm_NilRegistererSkips proves passing nil does not panic and
// keeps the adapter usable (e.g. for tests that only need the
// port.Metrics surface).
func TestProm_NilRegistererSkips(t *testing.T) {
	m := prommetrics.New(nil)
	m.IncCommitRetry(port.OutcomeSuccess)
	m.SetPendingEntries(1)
	m.SetReconciliationDriftPct("w", 0)
	m.SetOpenRouterDriftPct("m", 0)
}
