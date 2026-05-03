package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/wallet/adapter/memrepo"
	"github.com/pericles-luz/crm/internal/wallet/adapter/metrics/noop"
	"github.com/pericles-luz/crm/internal/wallet/adapter/queue/inmem"
	"github.com/pericles-luz/crm/internal/wallet/port"
	"github.com/pericles-luz/crm/internal/wallet/usecase"
)

// TestReconciliator_DriftAlertFires asserts AC #4: drift > 1% emits
// wallet.reconciliation_drift. We force drift by directly mutating the
// repo's stored balance after a successful commit, simulating a state
// where the ledger and balance_movement have diverged.
func TestReconciliator_DriftAlertFires(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 5, 1, 23, 0, 0, 0, time.UTC))
	repo := memrepo.New()
	repo.Now = clock.Now
	repo.SeedWallet(wallet.Wallet{ID: "w1", MasterID: "m1", BalanceMovement: 100})

	// Trigger drift artificially: stuck pending entry. Reserve subtracts
	// 50 from balance_movement (now 50), but commit never happens, so
	// sum_posted stays at 0 while balance_movement reflects the reserve.
	// residual = 0 + (100 - 50) = 50; drift = 50 / 100 = 50% — well over 1%.
	_ = reserveTokens(t, repo, clock, "w1", 50)

	alerter := &recAlerter{}
	metrics := noop.New()
	r := usecase.Reconciliator{
		Repo: repo, Queue: inmem.New(0), Metrics: metrics, Alerter: alerter, Clock: clock,
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("reconciliator: %v", err)
	}
	if !alerter.HasCode("wallet.reconciliation_drift") {
		t.Fatalf("expected drift alert; got %+v", alerter.Snapshot())
	}
	snap := metrics.Snapshot()
	if snap.DriftByWallet["w1"] <= 0.01 {
		t.Fatalf("drift gauge: got %.4f, want > 0.01", snap.DriftByWallet["w1"])
	}
}

// TestReconciliator_NoDriftQuiet asserts AC #4 quiet path: a wallet
// where Reserve+Commit completed cleanly has zero drift.
func TestReconciliator_NoDriftQuiet(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 5, 1, 23, 0, 0, 0, time.UTC))
	repo := memrepo.New()
	repo.Now = clock.Now
	repo.SeedWallet(wallet.Wallet{ID: "w1", MasterID: "m1", BalanceMovement: 1000})
	entryID := reserveTokens(t, repo, clock, "w1", 100)
	_ = repo.Commit(context.Background(), entryID, clock.Now())
	// balance = 900 (reserve subtracted), sum_posted = -100,
	// residual = -100 + (1000 - 900) = 0; drift = 0.

	alerter := &recAlerter{}
	metrics := noop.New()
	r := usecase.Reconciliator{
		Repo: repo, Queue: inmem.New(0), Metrics: metrics, Alerter: alerter, Clock: clock,
		DriftAlertPct: 0.01,
	}
	_ = r.RunOnce(context.Background())
	for _, a := range alerter.Snapshot() {
		if a.Code == "wallet.reconciliation_drift" {
			t.Fatalf("expected no drift alert; got %+v", a)
		}
	}
	if got := metrics.Snapshot().DriftByWallet["w1"]; got != 0 {
		t.Fatalf("drift gauge: got %.4f, want 0", got)
	}
}

// TestReconciliator_NoDriftWithNonZeroInitialBalance is the regression
// test that catches Blocker 2: a wallet hydrated with a non-zero
// InitialBalance going through Reserve+Commit must produce zero drift.
// memrepo.SeedWallet defaults InitialBalance to BalanceMovement, mirroring
// the schema's genesis-grant convention; if the postgres adapter ever
// stops loading initial_balance again, RunOnce fires drift on every
// healthy wallet — exactly the production failure the CTO flagged.
func TestReconciliator_NoDriftWithNonZeroInitialBalance(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 5, 1, 23, 0, 0, 0, time.UTC))
	repo := memrepo.New()
	repo.Now = clock.Now
	repo.SeedWallet(wallet.Wallet{ID: "w1", MasterID: "m1", InitialBalance: 1000, BalanceMovement: 1000})

	entryID := reserveTokens(t, repo, clock, "w1", 100)
	if err := repo.Commit(context.Background(), entryID, clock.Now()); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	alerter := &recAlerter{}
	metrics := noop.New()
	r := usecase.Reconciliator{
		Repo: repo, Queue: inmem.New(0), Metrics: metrics, Alerter: alerter, Clock: clock,
		DriftAlertPct: 0.01,
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("reconciliator: %v", err)
	}
	for _, a := range alerter.Snapshot() {
		if a.Code == "wallet.reconciliation_drift" {
			t.Fatalf("non-zero initial_balance steady-state must not alert; got %+v", a)
		}
	}
	if got := metrics.Snapshot().DriftByWallet["w1"]; got != 0 {
		t.Fatalf("drift gauge with InitialBalance=1000: got %.4f, want 0", got)
	}
	w, err := repo.GetWallet(context.Background(), "w1")
	if err != nil {
		t.Fatalf("get wallet: %v", err)
	}
	if w.InitialBalance != 1000 {
		t.Fatalf("InitialBalance not preserved by repo: got %d, want 1000", w.InitialBalance)
	}
}

// TestReconciliator_RescuesStuckPending exercises AC #7 "Worker crash
// injetado entre LLM-OK e commit → entry permanece pending →
// reconciliação noturna ... balance final correto".
func TestReconciliator_RescuesStuckPending(t *testing.T) {
	start := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	repo := memrepo.New()
	repo.Now = clock.Now
	repo.SeedWallet(wallet.Wallet{ID: "w1", MasterID: "m1", BalanceMovement: 500})

	entryID := reserveTokens(t, repo, clock, "w1", 100)
	// Simulate "worker crash between LLM-OK and commit" by leaving the
	// entry pending and advancing the clock past the threshold.
	clock.Advance(2 * time.Hour)

	queue := inmem.New(0)
	r := usecase.Reconciliator{
		Repo: repo, Queue: queue, Metrics: noop.New(), Alerter: &recAlerter{}, Clock: clock,
		PendingThreshold: 1 * time.Hour,
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("reconciliator: %v", err)
	}
	if queue.Len() != 1 {
		t.Fatalf("expected stuck pending re-enqueued; got queue depth %d", queue.Len())
	}

	// Drive the worker once to actually commit the rescued entry.
	worker := usecase.ReconcileWorker{
		Repo: repo, Queue: queue, Metrics: noop.New(), Alerter: &recAlerter{}, Clock: clock,
		Backoffs: []time.Duration{1 * time.Millisecond},
	}
	worker.RunOnce(context.Background())

	w, _ := repo.GetWallet(context.Background(), "w1")
	if w.BalanceMovement != 400 {
		t.Fatalf("rescued entry: want balance 400 (reserve already debited), got %d", w.BalanceMovement)
	}
	// entry should be posted now.
	e, _ := repo.GetEntry(context.Background(), entryID)
	if e.Status != wallet.StatusPosted {
		t.Fatalf("rescued status: got %s, want posted", e.Status)
	}
}

// fakeOpenRouter is a deterministic OpenRouter cost API for tests.
type fakeOpenRouter struct{ tokens map[string]int64 }

func (f fakeOpenRouter) DailyUsage(_ context.Context, masterID string, _ time.Time) (port.OpenRouterCostSample, error) {
	return port.OpenRouterCostSample{MasterID: masterID, Tokens: f.tokens[masterID]}, nil
}

// TestReconciliator_OpenRouterDrift covers AC #5: an OpenRouter sample
// that diverges from our ledger by more than 5% emits the alert.
func TestReconciliator_OpenRouterDrift(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 5, 1, 23, 0, 0, 0, time.UTC))
	repo := memrepo.New()
	repo.Now = clock.Now
	repo.SeedWallet(wallet.Wallet{ID: "w1", MasterID: "m1", BalanceMovement: 500})
	entryID := reserveTokens(t, repo, clock, "w1", 100)
	_ = repo.Commit(context.Background(), entryID, clock.Now())
	// Internal debit recorded = 100 tokens. OpenRouter says 1000 — large drift.

	alerter := &recAlerter{}
	metrics := noop.New()
	r := usecase.Reconciliator{
		Repo: repo, Queue: inmem.New(0), Metrics: metrics, Alerter: alerter, Clock: clock,
		OpenRouter: fakeOpenRouter{tokens: map[string]int64{"m1": 1000}},
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("reconciliator: %v", err)
	}
	if !alerter.HasCode("wallet.openrouter_drift") {
		t.Fatalf("expected openrouter_drift alert; got %+v", alerter.Snapshot())
	}
	if metrics.Snapshot().DriftByMaster["m1"] <= 0 {
		t.Fatalf("openrouter drift gauge missing")
	}
}

// errorOpenRouter returns errors so the reconciliator's error-path is exercised.
type errorOpenRouter struct{}

func (errorOpenRouter) DailyUsage(_ context.Context, _ string, _ time.Time) (port.OpenRouterCostSample, error) {
	return port.OpenRouterCostSample{}, errors.New("synthetic: openrouter unavailable")
}

// TestReconciliator_OpenRouterError makes sure a bad OpenRouter adapter
// does not crash the pass; the residual is tracked but no alert fires
// for that master.
func TestReconciliator_OpenRouterError(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 5, 1, 23, 0, 0, 0, time.UTC))
	repo := memrepo.New()
	repo.Now = clock.Now
	repo.SeedWallet(wallet.Wallet{ID: "w1", MasterID: "m1", BalanceMovement: 1000})

	alerter := &recAlerter{}
	r := usecase.Reconciliator{
		Repo: repo, Queue: inmem.New(0), Metrics: noop.New(), Alerter: alerter, Clock: clock,
		OpenRouter: errorOpenRouter{},
	}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("expected first-error surfaced; got nil")
	}
	for _, a := range alerter.Snapshot() {
		if a.Code == "wallet.openrouter_drift" {
			t.Fatalf("openrouter error path must not fire drift alert; got %+v", a)
		}
	}
}

// TestReconciliator_PendingGauge ensures the pending-entries gauge updates.
func TestReconciliator_PendingGauge(t *testing.T) {
	clock := newFakeClock(time.Now())
	repo := memrepo.New()
	repo.Now = clock.Now
	repo.SeedWallet(wallet.Wallet{ID: "w1", MasterID: "m", BalanceMovement: 500})
	_ = reserveTokens(t, repo, clock, "w1", 10)
	_ = reserveTokens(t, repo, clock, "w1", 20)

	metrics := noop.New()
	r := usecase.Reconciliator{Repo: repo, Queue: inmem.New(0), Metrics: metrics, Alerter: &recAlerter{}, Clock: clock}
	_ = r.RunOnce(context.Background())
	if metrics.Snapshot().Pending != 2 {
		t.Fatalf("pending gauge: got %d, want 2", metrics.Snapshot().Pending)
	}
}
