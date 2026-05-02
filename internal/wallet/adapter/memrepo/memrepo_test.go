package memrepo_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/wallet/adapter/memrepo"
)

// TestRepo_ReserveAtomicVsRace asserts F30 — concurrent Reserves
// against a wallet with insufficient combined balance must NOT both
// succeed. Single-mutex serialisation guarantees this; the test makes
// the contract explicit.
func TestRepo_ReserveAtomicVsRace(t *testing.T) {
	r := memrepo.New()
	r.SeedWallet(wallet.Wallet{ID: "w", MasterID: "m", BalanceMovement: 100})
	const goroutines = 8
	const amount = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	results := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, err := r.Reserve(context.Background(), "w", wallet.LedgerEntry{
				WalletID: "w", Kind: wallet.KindDebit, Source: wallet.SourceLLMCall,
				Amount: amount, CreatedAt: time.Now(),
			})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	insuf := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, wallet.ErrInsufficientFunds):
			insuf++
		default:
			t.Errorf("unexpected: %v", err)
		}
	}
	// 100 / 50 = 2 reservations should succeed, the rest should be insufficient.
	if successes != 2 {
		t.Errorf("successful reserves: got %d, want 2", successes)
	}
	if insuf != goroutines-2 {
		t.Errorf("insufficient: got %d, want %d", insuf, goroutines-2)
	}
}

// TestRepo_FailureInjection covers the Fail* knobs used by the
// regression scenarios (DB unavailable, persistent failure).
func TestRepo_FailureInjection(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	r := memrepo.New()
	r.Now = func() time.Time { return now }
	r.SeedWallet(wallet.Wallet{ID: "w", MasterID: "m", BalanceMovement: 1000})

	// FailNext fires once.
	r.FailNext()
	_, err := r.Reserve(context.Background(), "w", wallet.LedgerEntry{
		WalletID: "w", Kind: wallet.KindDebit, Source: wallet.SourceLLMCall, Amount: 10,
	})
	if !errors.Is(err, memrepo.ErrInjected) {
		t.Fatalf("FailNext: got %v", err)
	}
	// Next call works.
	if _, err := r.Reserve(context.Background(), "w", wallet.LedgerEntry{
		WalletID: "w", Kind: wallet.KindDebit, Source: wallet.SourceLLMCall, Amount: 10,
	}); err != nil {
		t.Fatalf("post-FailNext: %v", err)
	}

	// FailFor — within window fails, outside window succeeds.
	r.FailFor(2 * time.Second)
	_, err = r.Reserve(context.Background(), "w", wallet.LedgerEntry{
		WalletID: "w", Kind: wallet.KindDebit, Source: wallet.SourceLLMCall, Amount: 10,
	})
	if !errors.Is(err, memrepo.ErrInjected) {
		t.Fatalf("FailFor: got %v", err)
	}
	r.Now = func() time.Time { return now.Add(3 * time.Second) }
	if _, err := r.Reserve(context.Background(), "w", wallet.LedgerEntry{
		WalletID: "w", Kind: wallet.KindDebit, Source: wallet.SourceLLMCall, Amount: 10,
	}); err != nil {
		t.Fatalf("after FailFor window: %v", err)
	}

	// FailPermanently — every call fails until cleared.
	r.FailPermanently(true)
	if err := r.Commit(context.Background(), "x", now); !errors.Is(err, memrepo.ErrInjected) {
		t.Fatalf("permanent: got %v", err)
	}
	r.FailPermanently(false)
}

// TestRepo_GetMissingAndIncrementMissing covers small read paths.
func TestRepo_GetMissingAndIncrementMissing(t *testing.T) {
	r := memrepo.New()
	if _, err := r.GetWallet(context.Background(), "missing"); !errors.Is(err, wallet.ErrWalletNotFound) {
		t.Fatalf("GetWallet missing: %v", err)
	}
	if _, err := r.GetEntry(context.Background(), "missing"); !errors.Is(err, wallet.ErrEntryNotFound) {
		t.Fatalf("GetEntry missing: %v", err)
	}
	if err := r.IncrementAttempts(context.Background(), "missing"); !errors.Is(err, wallet.ErrEntryNotFound) {
		t.Fatalf("IncrementAttempts missing: %v", err)
	}
}

// TestRepo_CommitCancelGuards covers domain-terminal branches.
func TestRepo_CommitCancelGuards(t *testing.T) {
	r := memrepo.New()
	r.SeedWallet(wallet.Wallet{ID: "w", MasterID: "m", BalanceMovement: 500})
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return now }

	// Reserve then commit twice → second commit terminal.
	entry, err := r.Reserve(context.Background(), "w", wallet.LedgerEntry{
		WalletID: "w", Kind: wallet.KindDebit, Source: wallet.SourceLLMCall, Amount: 100, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := r.Commit(context.Background(), entry.ID, now); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := r.Commit(context.Background(), entry.ID, now); !errors.Is(err, wallet.ErrEntryAlreadyResolved) {
		t.Fatalf("double commit: %v", err)
	}

	// Cancel after commit → terminal.
	if err := r.Cancel(context.Background(), entry.ID, now); !errors.Is(err, wallet.ErrEntryAlreadyResolved) {
		t.Fatalf("cancel posted: %v", err)
	}

	// Commit on missing.
	if err := r.Commit(context.Background(), "missing", now); !errors.Is(err, wallet.ErrEntryNotFound) {
		t.Fatalf("commit missing: %v", err)
	}
	if err := r.Cancel(context.Background(), "missing", now); !errors.Is(err, wallet.ErrEntryNotFound) {
		t.Fatalf("cancel missing: %v", err)
	}
}

// TestRepo_ListPendingOlderThan_AndSums covers the reconciliator-facing reads.
func TestRepo_ListPendingOlderThan_AndSums(t *testing.T) {
	r := memrepo.New()
	r.SeedWallet(wallet.Wallet{ID: "w", MasterID: "m", BalanceMovement: 1000})
	old := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	young := old.Add(2 * time.Hour)

	r.Now = func() time.Time { return old }
	if _, err := r.Reserve(context.Background(), "w", wallet.LedgerEntry{
		WalletID: "w", Kind: wallet.KindDebit, Source: wallet.SourceLLMCall, Amount: 100,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r.Now = func() time.Time { return young }
	if _, err := r.Reserve(context.Background(), "w", wallet.LedgerEntry{
		WalletID: "w", Kind: wallet.KindDebit, Source: wallet.SourceLLMCall, Amount: 50,
	}); err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	pending, err := r.ListPendingOlderThan(context.Background(), young.Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d, want 1 pending older than", len(pending))
	}
	cnt, _ := r.CountPending(context.Background())
	if cnt != 2 {
		t.Fatalf("count pending: got %d", cnt)
	}
	wallets, _ := r.ListWallets(context.Background())
	if len(wallets) != 1 {
		t.Fatalf("list wallets: got %d", len(wallets))
	}
	sum, _ := r.SumPostedByWallet(context.Background(), "w")
	if sum != 0 {
		t.Fatalf("posted sum (none posted): %d", sum)
	}
	// IncrementAttempts happy path.
	for _, e := range pending {
		if err := r.IncrementAttempts(context.Background(), e.ID); err != nil {
			t.Fatalf("inc: %v", err)
		}
	}
}

// TestRepo_CancelHappyRestoresBalance covers the F30 rollback semantics:
// a successful cancel restores the reserved tokens to balance_movement.
func TestRepo_CancelHappyRestoresBalance(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	r := memrepo.New()
	r.Now = func() time.Time { return now }
	r.SeedWallet(wallet.Wallet{ID: "w", MasterID: "m", BalanceMovement: 200})
	entry, err := r.Reserve(context.Background(), "w", wallet.LedgerEntry{
		WalletID: "w", Kind: wallet.KindDebit, Source: wallet.SourceLLMCall, Amount: 100,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	w, _ := r.GetWallet(context.Background(), "w")
	if w.BalanceMovement != 100 {
		t.Fatalf("post-reserve balance: got %d", w.BalanceMovement)
	}
	if err := r.Cancel(context.Background(), entry.ID, now); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	w, _ = r.GetWallet(context.Background(), "w")
	if w.BalanceMovement != 200 {
		t.Fatalf("post-cancel balance: got %d, want 200 (restored)", w.BalanceMovement)
	}

	// Cancel injection — covers the shouldFail branch.
	entry2, err := r.Reserve(context.Background(), "w", wallet.LedgerEntry{
		WalletID: "w", Kind: wallet.KindDebit, Source: wallet.SourceLLMCall, Amount: 50,
	})
	if err != nil {
		t.Fatalf("reserve 2: %v", err)
	}
	r.FailNext()
	if err := r.Cancel(context.Background(), entry2.ID, now); !errors.Is(err, memrepo.ErrInjected) {
		t.Fatalf("cancel injected: got %v", err)
	}
}

// TestRepo_ContextCancelled ensures the cancel-context branch returns ctx.Err().
func TestRepo_ContextCancelled(t *testing.T) {
	r := memrepo.New()
	r.SeedWallet(wallet.Wallet{ID: "w", MasterID: "m", BalanceMovement: 100})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Reserve(ctx, "w", wallet.LedgerEntry{
		WalletID: "w", Kind: wallet.KindDebit, Source: wallet.SourceLLMCall, Amount: 1,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	if err := r.Commit(ctx, "x", time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("commit canceled: got %v", err)
	}
	if err := r.Cancel(ctx, "x", time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel canceled: got %v", err)
	}
}
