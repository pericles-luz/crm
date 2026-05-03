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
	m.RetainReconciliationDriftLabels([]string{"w"})
	m.RetainOpenRouterDriftLabels([]string{"m"})
}

// driftSeries returns the wallet_id label values currently exported on
// wallet_reconciliation_drift_pct.
func driftSeries(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "wallet_reconciliation_drift_pct" {
			continue
		}
		for _, mt := range mf.GetMetric() {
			for _, lp := range mt.GetLabel() {
				if lp.GetName() == "wallet_id" {
					out[lp.GetValue()] = mt.GetGauge().GetValue()
				}
			}
		}
	}
	return out
}

// openrouterSeries returns the master_id label values currently exported
// on wallet_openrouter_drift_pct.
func openrouterSeries(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "wallet_openrouter_drift_pct" {
			continue
		}
		for _, mt := range mf.GetMetric() {
			for _, lp := range mt.GetLabel() {
				if lp.GetName() == "master_id" {
					out[lp.GetValue()] = mt.GetGauge().GetValue()
				}
			}
		}
	}
	return out
}

// TestProm_RetainReconciliationDriftLabels proves SIN-62269 AC #1+#3:
// after registering metrics for two wallets, dropping one from the
// active set and calling Retain… deletes that wallet's gauge series so
// it is no longer exported by the registry.
func TestProm_RetainReconciliationDriftLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := prommetrics.New(reg)

	m.SetReconciliationDriftPct("w1", 0.01)
	m.SetReconciliationDriftPct("w2", 0.02)

	got := driftSeries(t, reg)
	if _, ok := got["w1"]; !ok {
		t.Fatalf("w1 series missing before prune: %v", got)
	}
	if _, ok := got["w2"]; !ok {
		t.Fatalf("w2 series missing before prune: %v", got)
	}

	// Drop w2 from the active set: w1 remains, w2 must be deleted.
	m.RetainReconciliationDriftLabels([]string{"w1"})

	got = driftSeries(t, reg)
	if _, ok := got["w2"]; ok {
		t.Fatalf("w2 series still exported after prune: %v", got)
	}
	if v, ok := got["w1"]; !ok || v != 0.01 {
		t.Fatalf("w1 series lost or mutated: ok=%v v=%v", ok, v)
	}

	// Re-setting after a prune must re-register the series cleanly
	// (covers the pruned-then-re-seen wallet path).
	m.SetReconciliationDriftPct("w2", 0.05)
	got = driftSeries(t, reg)
	if v := got["w2"]; v != 0.05 {
		t.Fatalf("w2 series not re-registered after prune: got %v", v)
	}

	// Empty active set drops every series.
	m.RetainReconciliationDriftLabels(nil)
	got = driftSeries(t, reg)
	if len(got) != 0 {
		t.Fatalf("nil active set must drop all series; got %v", got)
	}
}

// TestProm_RetainOpenRouterDriftLabels mirrors AC #2 for the master_id
// label on wallet_openrouter_drift_pct.
func TestProm_RetainOpenRouterDriftLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := prommetrics.New(reg)

	m.SetOpenRouterDriftPct("m1", 0.03)
	m.SetOpenRouterDriftPct("m2", 0.04)

	got := openrouterSeries(t, reg)
	if _, ok := got["m1"]; !ok {
		t.Fatalf("m1 series missing before prune: %v", got)
	}
	if _, ok := got["m2"]; !ok {
		t.Fatalf("m2 series missing before prune: %v", got)
	}

	m.RetainOpenRouterDriftLabels([]string{"m1"})

	got = openrouterSeries(t, reg)
	if _, ok := got["m2"]; ok {
		t.Fatalf("m2 series still exported after prune: %v", got)
	}
	if v, ok := got["m1"]; !ok || v != 0.03 {
		t.Fatalf("m1 series lost or mutated: ok=%v v=%v", ok, v)
	}
}

// TestProm_RetainAllActiveKeepsSeries proves Retain is idempotent when
// the active set fully covers the seen ids — the registry must not
// drop anything and the gauge values must survive.
func TestProm_RetainAllActiveKeepsSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := prommetrics.New(reg)

	m.SetReconciliationDriftPct("w1", 0.01)
	m.SetReconciliationDriftPct("w2", 0.02)
	m.RetainReconciliationDriftLabels([]string{"w1", "w2", "w3-not-seen"})

	got := driftSeries(t, reg)
	if got["w1"] != 0.01 || got["w2"] != 0.02 {
		t.Fatalf("expected w1=0.01,w2=0.02 retained; got %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("retain must not introduce new series; got %v", got)
	}
}
