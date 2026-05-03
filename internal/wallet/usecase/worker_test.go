package usecase_test

import (
	"context"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/wallet/adapter/memrepo"
	"github.com/pericles-luz/crm/internal/wallet/adapter/metrics/noop"
	"github.com/pericles-luz/crm/internal/wallet/adapter/queue/inmem"
	"github.com/pericles-luz/crm/internal/wallet/port"
	"github.com/pericles-luz/crm/internal/wallet/usecase"
)

// TestWorker_DrainsAndCommits asserts AC #3: the worker pulls a job
// off wallet.reconcile_pending and commits successfully.
func TestWorker_DrainsAndCommits(t *testing.T) {
	clock := newFakeClock(time.Now())
	repo := memrepo.New()
	repo.Now = clock.Now
	repo.SeedWallet(wallet.Wallet{ID: "w1", MasterID: "m", BalanceMovement: 500})
	entryID := reserveTokens(t, repo, clock, "w1", 100)

	queue := inmem.New(0)
	if err := queue.Enqueue(context.Background(), port.ReconcileJob{EntryID: entryID, WalletID: "w1"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	alerter := &recAlerter{}
	w := usecase.ReconcileWorker{
		Repo: repo, Queue: queue, Metrics: noop.New(), Alerter: alerter,
		Clock: clock, Backoffs: []time.Duration{1 * time.Millisecond},
	}
	w.RunOnce(context.Background())

	wt, _ := repo.GetWallet(context.Background(), "w1")
	if wt.BalanceMovement != 400 {
		t.Fatalf("worker did not commit; balance %d (Reserve already debited; Commit just flips status)", wt.BalanceMovement)
	}
	if len(alerter.Snapshot()) != 0 {
		t.Fatalf("no alerts expected on success path; got %v", alerter.Snapshot())
	}
}

// TestWorker_EscalatesAfterThreshold asserts AC #3: after EscalateAfter
// worker-side attempts on the same job, the worker emits the persistent-
// failure alert. job.Attempts (cumulative inline retries) does NOT count
// toward the threshold — see the SIN-62240 review smaller-finding 2.
func TestWorker_EscalatesAfterThreshold(t *testing.T) {
	clock := newFakeClock(time.Now())
	repo := memrepo.New()
	repo.Now = clock.Now
	repo.SeedWallet(wallet.Wallet{ID: "w1", MasterID: "m", BalanceMovement: 500})
	entryID := reserveTokens(t, repo, clock, "w1", 100)
	repo.FailPermanently(true)

	queue := inmem.New(0)
	// job.Attempts = 3 (inline retries already exhausted) — must NOT
	// count toward the worker's threshold.
	_ = queue.Enqueue(context.Background(), port.ReconcileJob{EntryID: entryID, WalletID: "w1", Attempts: 3})

	alerter := &recAlerter{}
	metrics := noop.New()
	w := usecase.ReconcileWorker{
		Repo: repo, Queue: queue, Metrics: metrics, Alerter: alerter,
		Clock: clock, Backoffs: []time.Duration{1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond},
		EscalateAfter: 2, // 2 worker-side attempts → alert.
	}
	w.RunOnce(context.Background())

	if !alerter.HasCode("wallet.commit_persistent_failure") {
		t.Fatalf("expected persistent-failure alert; got %+v", alerter.Snapshot())
	}
	snap := metrics.Snapshot()
	if snap.CommitRetry[port.OutcomeExhausted] == 0 {
		t.Fatalf("exhausted metric not incremented")
	}
	// Two worker attempts must each have been a real Commit call —
	// proving job.Attempts did not short-circuit the retry budget.
	if got := snap.CommitRetry[port.OutcomeRetry]; got < 1 {
		t.Fatalf("worker must retry at least once before escalating; got %d retry metrics", got)
	}
}

// TestWorker_RetriesBeforeEscalating regression-tests reviewer
// smaller-finding 2: a job arriving with cumulative job.Attempts at or
// over the inline-retry exhaustion count must still get the worker's
// full retry budget. With the old logic that counted cumulative
// attempts, a job with Attempts=4 alerted on the very first worker
// attempt. Under the fix, the first worker attempt fails, the second
// succeeds, and no alert fires.
func TestWorker_RetriesBeforeEscalating(t *testing.T) {
	clock := newFakeClock(time.Now())
	repo := memrepo.New()
	repo.Now = clock.Now
	repo.SeedWallet(wallet.Wallet{ID: "w1", MasterID: "m", BalanceMovement: 500})
	entryID := reserveTokens(t, repo, clock, "w1", 100)

	queue := inmem.New(0)
	_ = queue.Enqueue(context.Background(), port.ReconcileJob{EntryID: entryID, WalletID: "w1", Attempts: 4})

	repo.FailNext() // first worker attempt fails; the second commits.

	alerter := &recAlerter{}
	metrics := noop.New()
	w := usecase.ReconcileWorker{
		Repo: repo, Queue: queue, Metrics: metrics, Alerter: alerter,
		Clock:         clock,
		Backoffs:      []time.Duration{1 * time.Millisecond, 1 * time.Millisecond},
		EscalateAfter: 5,
	}
	w.RunOnce(context.Background())

	if alerter.HasCode("wallet.commit_persistent_failure") {
		t.Fatalf("worker must retry before alerting (job.Attempts=4 cumulative, EscalateAfter=5 worker-side); got %+v", alerter.Snapshot())
	}
	e, _ := repo.GetEntry(context.Background(), entryID)
	if e.Status != wallet.StatusPosted {
		t.Fatalf("after retry, entry must be posted; got %s", e.Status)
	}
	snap := metrics.Snapshot()
	if snap.CommitRetry[port.OutcomeRetry] == 0 {
		t.Fatalf("expected at least one retry metric; got %+v", snap.CommitRetry)
	}
	if snap.CommitRetry[port.OutcomeSuccess] == 0 {
		t.Fatalf("expected success on second attempt; got %+v", snap.CommitRetry)
	}
}

// TestWorker_TerminatesOnAlreadyResolved exercises the "domain
// terminal" branch — if another writer already committed/cancelled the
// entry, the worker stops without alerting.
func TestWorker_TerminatesOnAlreadyResolved(t *testing.T) {
	clock := newFakeClock(time.Now())
	repo := memrepo.New()
	repo.Now = clock.Now
	repo.SeedWallet(wallet.Wallet{ID: "w1", MasterID: "m", BalanceMovement: 500})
	entryID := reserveTokens(t, repo, clock, "w1", 100)
	if err := repo.Commit(context.Background(), entryID, clock.Now()); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	queue := inmem.New(0)
	_ = queue.Enqueue(context.Background(), port.ReconcileJob{EntryID: entryID, WalletID: "w1"})
	alerter := &recAlerter{}
	metrics := noop.New()
	w := usecase.ReconcileWorker{Repo: repo, Queue: queue, Metrics: metrics, Alerter: alerter, Clock: clock,
		Backoffs: []time.Duration{1 * time.Millisecond}}
	w.RunOnce(context.Background())
	if len(alerter.Snapshot()) != 0 {
		t.Fatalf("no alert expected; got %+v", alerter.Snapshot())
	}
	if metrics.Snapshot().CommitRetry[port.OutcomeExhausted] != 1 {
		t.Fatalf("expected exhausted metric on terminal-domain branch")
	}
}

// TestWorker_RunHonoursContextCancellation makes sure Run exits cleanly.
func TestWorker_RunHonoursContextCancellation(t *testing.T) {
	repo := memrepo.New()
	queue := inmem.New(0)
	w := usecase.ReconcileWorker{Repo: repo, Queue: queue, Metrics: noop.New(), Alerter: &recAlerter{}, Clock: newFakeClock(time.Now())}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run on cancelled ctx: %v", err)
	}
}
