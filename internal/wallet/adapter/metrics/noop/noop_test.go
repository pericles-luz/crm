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
