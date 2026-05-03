//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/wallet"
	pgrepo "github.com/pericles-luz/crm/internal/wallet/adapter/postgres"
)

// connect honours WALLET_PG_DSN. Tests skip when unset so that
// `go test -tags integration ./...` is opt-in based on env, not on
// whether docker-compose happens to be up locally.
func connect(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WALLET_PG_DSN")
	if dsn == "" {
		t.Skip("WALLET_PG_DSN not set; skipping postgres integration test")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// truncate clears the wallet tables between tests.
func truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "TRUNCATE token_ledger, token_wallets RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// TestReserveCommitCancel exercises the happy path against real Postgres.
func TestReserveCommitCancel(t *testing.T) {
	pool := connect(t)
	truncate(t, pool)
	r := pgrepo.New(pool)
	ctx := context.Background()

	walletID := uuid.NewString()
	masterID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO token_wallets (id, master_id, initial_balance, balance_movement) VALUES ($1, $2, 1000, 1000)`,
		walletID, masterID,
	); err != nil {
		t.Fatalf("seed wallet: %v", err)
	}

	// Reserve.
	persisted, err := r.Reserve(ctx, walletID, wallet.LedgerEntry{
		WalletID:  walletID,
		Kind:      wallet.KindDebit,
		Source:    wallet.SourceLLMCall,
		Amount:    100,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	w, err := r.GetWallet(ctx, walletID)
	if err != nil {
		t.Fatalf("get wallet: %v", err)
	}
	// F30 contract: Reserve atomically debits balance_movement.
	if w.BalanceMovement != 900 {
		t.Fatalf("F30 atomic reserve must subtract amount; got balance %d, want 900", w.BalanceMovement)
	}

	// Commit is a status flip only; balance is unchanged because Reserve
	// already debited it.
	if err := r.Commit(ctx, persisted.ID, time.Now().UTC()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	w, _ = r.GetWallet(ctx, walletID)
	if w.BalanceMovement != 900 {
		t.Fatalf("commit must NOT change balance_movement; got %d, want 900", w.BalanceMovement)
	}

	// Re-commit must be terminal.
	err = r.Commit(ctx, persisted.ID, time.Now().UTC())
	if !errors.Is(err, wallet.ErrEntryAlreadyResolved) {
		t.Fatalf("double commit: want ErrEntryAlreadyResolved, got %v", err)
	}

	// Reserve + cancel — Reserve debits 50, Cancel restores 50, so the
	// wallet should be back at 900.
	pending, err := r.Reserve(ctx, walletID, wallet.LedgerEntry{
		WalletID:  walletID,
		Kind:      wallet.KindDebit,
		Source:    wallet.SourceLLMCall,
		Amount:    50,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("reserve 2: %v", err)
	}
	w, _ = r.GetWallet(ctx, walletID)
	if w.BalanceMovement != 850 {
		t.Fatalf("reserve 2 must subtract amount; got %d, want 850", w.BalanceMovement)
	}
	if err := r.Cancel(ctx, pending.ID, time.Now().UTC()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	w, _ = r.GetWallet(ctx, walletID)
	if w.BalanceMovement != 900 {
		t.Fatalf("cancel must restore reserved tokens; got %d, want 900", w.BalanceMovement)
	}

	// Drift sum + count.
	sum, err := r.SumPostedByWallet(ctx, walletID)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if sum != -100 {
		t.Fatalf("posted sum: want -100, got %d", sum)
	}
	if cnt, err := r.CountPending(ctx); err != nil || cnt != 0 {
		t.Fatalf("count pending: want 0 nil, got %d %v", cnt, err)
	}
}

// TestReserveInsufficientFunds asserts F30 atomic guard rejects an
// over-debit when balance is too low.
func TestReserveInsufficientFunds(t *testing.T) {
	pool := connect(t)
	truncate(t, pool)
	r := pgrepo.New(pool)
	ctx := context.Background()

	walletID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO token_wallets (id, master_id, initial_balance, balance_movement) VALUES ($1, $2, 50, 50)`,
		walletID, uuid.NewString(),
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := r.Reserve(ctx, walletID, wallet.LedgerEntry{
		WalletID:  walletID,
		Kind:      wallet.KindDebit,
		Source:    wallet.SourceLLMCall,
		Amount:    100,
		CreatedAt: time.Now().UTC(),
	})
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("want ErrInsufficientFunds, got %v", err)
	}
}

// TestReserveAtomicVsRace_Postgres is the production-side counterpart to
// memrepo's TestRepo_ReserveAtomicVsRace. With balance=100 and N=8
// goroutines each reserving 50, the F30 atomic-reserve contract requires
// exactly 2 successes and 6 ErrInsufficientFunds — proving that
// SELECT FOR UPDATE + balance_movement decrement inside the same TX
// serialises concurrent reservations against the real database.
func TestReserveAtomicVsRace_Postgres(t *testing.T) {
	pool := connect(t)
	truncate(t, pool)
	r := pgrepo.New(pool)
	ctx := context.Background()

	walletID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO token_wallets (id, master_id, initial_balance, balance_movement) VALUES ($1, $2, 100, 100)`,
		walletID, uuid.NewString(),
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const goroutines = 8
	const amount = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	results := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, err := r.Reserve(ctx, walletID, wallet.LedgerEntry{
				WalletID:  walletID,
				Kind:      wallet.KindDebit,
				Source:    wallet.SourceLLMCall,
				Amount:    amount,
				CreatedAt: time.Now().UTC(),
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
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successes != 2 {
		t.Errorf("F30 race: successful reserves = %d, want 2", successes)
	}
	if insuf != goroutines-2 {
		t.Errorf("F30 race: insufficient = %d, want %d", insuf, goroutines-2)
	}

	// Final balance must be exactly 0 — two debits of 50 against 100.
	w, err := r.GetWallet(ctx, walletID)
	if err != nil {
		t.Fatalf("get wallet: %v", err)
	}
	if w.BalanceMovement != 0 {
		t.Fatalf("F30 race: post-race balance = %d, want 0", w.BalanceMovement)
	}
}

// TestWalletInitialBalanceLoaded is the Blocker 2 regression test:
// GetWallet and ListWallets must hydrate Wallet.InitialBalance from the
// schema, otherwise the reconciliator's drift formula divides by 1 and
// fires a false-positive alert on every healthy wallet.
func TestWalletInitialBalanceLoaded(t *testing.T) {
	pool := connect(t)
	truncate(t, pool)
	r := pgrepo.New(pool)
	ctx := context.Background()

	walletID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO token_wallets (id, master_id, initial_balance, balance_movement) VALUES ($1, $2, 1000, 1000)`,
		walletID, uuid.NewString(),
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := r.GetWallet(ctx, walletID)
	if err != nil {
		t.Fatalf("get wallet: %v", err)
	}
	if got.InitialBalance != 1000 {
		t.Fatalf("GetWallet initial_balance: got %d, want 1000", got.InitialBalance)
	}

	list, err := r.ListWallets(ctx)
	if err != nil {
		t.Fatalf("list wallets: %v", err)
	}
	if len(list) != 1 || list[0].InitialBalance != 1000 {
		t.Fatalf("ListWallets initial_balance: got %+v, want one wallet with InitialBalance=1000", list)
	}
}

// TestReserveCommitDriftSteadyState pins the Blocker 2/3 invariant: a
// healthy Reserve+Commit cycle on a wallet with non-zero
// initial_balance must produce sum_posted + (initial - movement) == 0.
// Counterpart to memrepo's TestReconciliator_NoDriftQuiet against the
// real database.
func TestReserveCommitDriftSteadyState(t *testing.T) {
	pool := connect(t)
	truncate(t, pool)
	r := pgrepo.New(pool)
	ctx := context.Background()

	walletID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO token_wallets (id, master_id, initial_balance, balance_movement) VALUES ($1, $2, 1000, 1000)`,
		walletID, uuid.NewString(),
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	persisted, err := r.Reserve(ctx, walletID, wallet.LedgerEntry{
		WalletID:  walletID,
		Kind:      wallet.KindDebit,
		Source:    wallet.SourceLLMCall,
		Amount:    100,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := r.Commit(ctx, persisted.ID, time.Now().UTC()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	w, err := r.GetWallet(ctx, walletID)
	if err != nil {
		t.Fatalf("get wallet: %v", err)
	}
	sum, err := r.SumPostedByWallet(ctx, walletID)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	residual := sum + (w.InitialBalance - w.BalanceMovement)
	if residual != 0 {
		t.Fatalf("steady-state drift residual: got %d (sum=%d, initial=%d, movement=%d), want 0",
			residual, sum, w.InitialBalance, w.BalanceMovement)
	}
}
