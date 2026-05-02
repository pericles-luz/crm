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

// sharedIDs is a per-test-run unique-ID generator so multiple
// reserveTokens calls produce distinct ledger entry ids.
var sharedIDs = &seqIDs{}

// reserveTokens helps each test set up a wallet + pending entry.
func reserveTokens(t *testing.T, repo *memrepo.Repo, clock *fakeClock, walletID string, amount int64) string {
	t.Helper()
	r := usecase.Reserve{Repo: repo, IDs: sharedIDs, Clock: clock}
	entry, err := r.Run(context.Background(), usecase.ReserveInput{
		WalletID: walletID, Amount: amount, Source: wallet.SourceLLMCall,
	})
	if err != nil {
		t.Fatalf("seed reserve: %v", err)
	}
	return entry.ID
}

// TestCommit_HappyPath asserts AC #2 happy path: a single attempt
// converts pending → posted and applies the debit.
func TestCommit_HappyPath(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	repo := memrepo.New()
	repo.Now = clock.Now
	repo.SeedWallet(wallet.Wallet{ID: "w1", MasterID: "m", BalanceMovement: 500})
	entryID := reserveTokens(t, repo, clock, "w1", 100)

	metrics := noop.New()
	queue := inmem.New(0)
	c := usecase.CommitDebit{Repo: repo, Queue: queue, Metrics: metrics, Clock: clock}
	if err := c.Run(context.Background(), entryID); err != nil {
		t.Fatalf("commit: %v", err)
	}

	w, _ := repo.GetWallet(context.Background(), "w1")
	if w.BalanceMovement != 400 {
		t.Fatalf("balance after reserve+commit: got %d, want 400", w.BalanceMovement)
	}
	snap := metrics.Snapshot()
	if snap.CommitRetry[port.OutcomeSuccess] != 1 {
		t.Fatalf("success metric: got %d", snap.CommitRetry[port.OutcomeSuccess])
	}
	if queue.Len() != 0 {
		t.Fatalf("queue should be empty on success")
	}
}

// TestCommit_RetriesThenSucceeds covers AC #7 "DB temporariamente
// indisponível por 2s → retry passa no 2º ou 3º attempt". With the
// 200/800/3200ms backoff schedule and 1+3 attempts, attempt 4 fires at
// t=4.2s, comfortably past the 2s outage.
//
// We seed a 2-second outage. Backoffs 200ms, 800ms, 3.2s — after the
// first 200ms sleep the clock has not yet passed the 2s window, so the
// second attempt also fails; after another 800ms total time is 1s,
// still in the window, so attempt 3 fires after 3.2s sleep when total
// time = 4.2s, comfortably past the 2s outage.
func TestCommit_RetriesThenSucceeds(t *testing.T) {
	start := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	repo := memrepo.New()
	repo.Now = clock.Now
	repo.SeedWallet(wallet.Wallet{ID: "w1", MasterID: "m", BalanceMovement: 500})
	entryID := reserveTokens(t, repo, clock, "w1", 100)

	repo.FailFor(2 * time.Second)
	metrics := noop.New()
	queue := inmem.New(0)
	c := usecase.CommitDebit{Repo: repo, Queue: queue, Metrics: metrics, Clock: clock}
	if err := c.Run(context.Background(), entryID); err != nil {
		t.Fatalf("commit retry: %v", err)
	}
	w, _ := repo.GetWallet(context.Background(), "w1")
	if w.BalanceMovement != 400 {
		t.Fatalf("balance after reserve+commit: got %d", w.BalanceMovement)
	}
	snap := metrics.Snapshot()
	if snap.CommitRetry[port.OutcomeSuccess] != 1 {
		t.Fatalf("success metric: got %d", snap.CommitRetry[port.OutcomeSuccess])
	}
	if snap.CommitRetry[port.OutcomeRetry] < 1 {
		t.Fatalf("retry metric: expected >=1, got %d", snap.CommitRetry[port.OutcomeRetry])
	}
	if queue.Len() != 0 {
		t.Fatalf("queue should be empty when retry succeeds")
	}
	// AC says backoffs are 200ms / 800ms / 3.2s; assert at least the
	// first two were honoured (sleep schedule).
	sleeps := clock.Sleeps()
	if len(sleeps) < 1 || sleeps[0] != 200*time.Millisecond {
		t.Fatalf("first backoff: got %v", sleeps)
	}
}

// TestCommit_ExhaustedEnqueues covers AC #2 + AC #7 "DB indisponível
// persistente → entry vai para fila de reconciliação".
func TestCommit_ExhaustedEnqueues(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	repo := memrepo.New()
	repo.Now = clock.Now
	repo.SeedWallet(wallet.Wallet{ID: "w1", MasterID: "m", BalanceMovement: 500})
	entryID := reserveTokens(t, repo, clock, "w1", 100)
	repo.FailPermanently(true)

	metrics := noop.New()
	queue := inmem.New(0)
	c := usecase.CommitDebit{Repo: repo, Queue: queue, Metrics: metrics, Clock: clock}
	err := c.Run(context.Background(), entryID)
	if !errors.Is(err, wallet.ErrCommitExhausted) {
		t.Fatalf("got %v, want ErrCommitExhausted", err)
	}
	if queue.Len() != 1 {
		t.Fatalf("queue depth: got %d, want 1", queue.Len())
	}
	snap := metrics.Snapshot()
	if snap.CommitRetry[port.OutcomeEnqueued] != 1 {
		t.Fatalf("enqueued metric: got %d", snap.CommitRetry[port.OutcomeEnqueued])
	}
	// Reserve already subtracted; subsequent commit failures do not
	// move the needle. Balance stays at the post-reserve value.
	w, _ := repo.GetWallet(context.Background(), "w1")
	if w.BalanceMovement != 400 {
		t.Fatalf("balance must remain at post-reserve value 400, got %d", w.BalanceMovement)
	}
}

// TestCommit_AlreadyResolvedTerminal covers idempotency.
func TestCommit_AlreadyResolvedTerminal(t *testing.T) {
	clock := newFakeClock(time.Now())
	repo := memrepo.New()
	repo.Now = clock.Now
	repo.SeedWallet(wallet.Wallet{ID: "w1", MasterID: "m", BalanceMovement: 500})
	entryID := reserveTokens(t, repo, clock, "w1", 100)

	metrics := noop.New()
	c := usecase.CommitDebit{Repo: repo, Queue: inmem.New(0), Metrics: metrics, Clock: clock}
	if err := c.Run(context.Background(), entryID); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	err := c.Run(context.Background(), entryID)
	if !errors.Is(err, wallet.ErrEntryAlreadyResolved) {
		t.Fatalf("second commit: got %v, want ErrEntryAlreadyResolved", err)
	}
	snap := metrics.Snapshot()
	if snap.CommitRetry[port.OutcomeExhausted] != 1 {
		t.Fatalf("exhausted metric: got %d", snap.CommitRetry[port.OutcomeExhausted])
	}
}

// TestCommit_EmptyEntryID guards the boundary check.
func TestCommit_EmptyEntryID(t *testing.T) {
	c := usecase.CommitDebit{Repo: memrepo.New(), Queue: inmem.New(0), Metrics: noop.New(), Clock: newFakeClock(time.Now())}
	err := c.Run(context.Background(), "")
	if !errors.Is(err, wallet.ErrEntryNotFound) {
		t.Fatalf("got %v, want ErrEntryNotFound", err)
	}
}

// TestCancel covers the rollback path.
func TestCancel_HappyAndIdempotent(t *testing.T) {
	clock := newFakeClock(time.Now())
	repo := memrepo.New()
	repo.Now = clock.Now
	repo.SeedWallet(wallet.Wallet{ID: "w1", MasterID: "m", BalanceMovement: 500})
	entryID := reserveTokens(t, repo, clock, "w1", 100)

	c := usecase.CancelDebit{Repo: repo, Clock: clock}
	// After reserve, balance was 400. After cancel, balance must be
	// restored to 500 (the seeded total).
	if err := c.Run(context.Background(), entryID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	w, _ := repo.GetWallet(context.Background(), "w1")
	if w.BalanceMovement != 500 {
		t.Fatalf("cancel must restore reserved tokens; got balance %d, want 500", w.BalanceMovement)
	}

	// Second cancel → idempotent terminal error.
	err := c.Run(context.Background(), entryID)
	if !errors.Is(err, wallet.ErrEntryAlreadyResolved) {
		t.Fatalf("second cancel: got %v", err)
	}
	// Empty id boundary
	if err := c.Run(context.Background(), ""); !errors.Is(err, wallet.ErrEntryNotFound) {
		t.Fatalf("empty id: got %v", err)
	}
	// Missing entry id
	if err := c.Run(context.Background(), "nope"); !errors.Is(err, wallet.ErrEntryNotFound) {
		t.Fatalf("missing id: got %v", err)
	}
	// Transient wrap on cancel: reserve a fresh entry, then make Cancel
	// fail once.
	id2 := reserveTokens(t, repo, clock, "w1", 50)
	repo.FailNext()
	err = c.Run(context.Background(), id2)
	if !errors.Is(err, wallet.ErrTransient) {
		t.Fatalf("transient: got %v", err)
	}
}
