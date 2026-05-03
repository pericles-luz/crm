package noop_test

import (
	"testing"

	"github.com/pericles-luz/crm/internal/wallet/adapter/metrics/noop"
	"github.com/pericles-luz/crm/internal/wallet/port"
)

func TestMetrics_Snapshot(t *testing.T) {
	m := noop.New()
	m.IncCommitRetry(port.OutcomeSuccess)
	m.IncCommitRetry(port.OutcomeRetry)
	m.IncCommitRetry(port.OutcomeRetry)
	m.SetPendingEntries(7)
	m.SetReconciliationDriftPct("w1", 0.05)
	m.SetOpenRouterDriftPct("m1", 0.10)

	snap := m.Snapshot()
	if snap.CommitRetry[port.OutcomeSuccess] != 1 {
		t.Errorf("success: %d", snap.CommitRetry[port.OutcomeSuccess])
	}
	if snap.CommitRetry[port.OutcomeRetry] != 2 {
		t.Errorf("retry: %d", snap.CommitRetry[port.OutcomeRetry])
	}
	if snap.Pending != 7 {
		t.Errorf("pending: %d", snap.Pending)
	}
	if snap.DriftByWallet["w1"] != 0.05 {
		t.Errorf("drift wallet: %v", snap.DriftByWallet)
	}
	if snap.DriftByMaster["m1"] != 0.10 {
		t.Errorf("drift master: %v", snap.DriftByMaster)
	}
}

// TestMetrics_RetainDriftLabels covers SIN-62269: the noop adapter must
// expose the same prune contract as prom so reconciliator tests can
// assert dropped wallets/masters are no longer tracked.
func TestMetrics_RetainDriftLabels(t *testing.T) {
	m := noop.New()
	m.SetReconciliationDriftPct("w1", 0.01)
	m.SetReconciliationDriftPct("w2", 0.02)
	m.SetOpenRouterDriftPct("m1", 0.03)
	m.SetOpenRouterDriftPct("m2", 0.04)

	m.RetainReconciliationDriftLabels([]string{"w1"})
	m.RetainOpenRouterDriftLabels([]string{"m2"})

	snap := m.Snapshot()
	if _, ok := snap.DriftByWallet["w2"]; ok {
		t.Fatalf("w2 should be dropped: %v", snap.DriftByWallet)
	}
	if v := snap.DriftByWallet["w1"]; v != 0.01 {
		t.Fatalf("w1 mutated: %v", snap.DriftByWallet)
	}
	if _, ok := snap.DriftByMaster["m1"]; ok {
		t.Fatalf("m1 should be dropped: %v", snap.DriftByMaster)
	}
	if v := snap.DriftByMaster["m2"]; v != 0.04 {
		t.Fatalf("m2 mutated: %v", snap.DriftByMaster)
	}
}
