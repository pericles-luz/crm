//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"os"
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
		`INSERT INTO token_wallets (id, master_id, balance_movement) VALUES ($1, $2, 1000)`,
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
	if w.BalanceMovement != 1000 {
		t.Fatalf("reserve must NOT change balance_movement, got %d", w.BalanceMovement)
	}

	// Commit.
	if err := r.Commit(ctx, persisted.ID, time.Now().UTC()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	w, _ = r.GetWallet(ctx, walletID)
	if w.BalanceMovement != 900 {
		t.Fatalf("commit must apply debit; got balance %d, want 900", w.BalanceMovement)
	}

	// Re-commit must be terminal.
	err = r.Commit(ctx, persisted.ID, time.Now().UTC())
	if !errors.Is(err, wallet.ErrEntryAlreadyResolved) {
		t.Fatalf("double commit: want ErrEntryAlreadyResolved, got %v", err)
	}

	// Reserve + cancel.
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
	if err := r.Cancel(ctx, pending.ID, time.Now().UTC()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	w, _ = r.GetWallet(ctx, walletID)
	if w.BalanceMovement != 900 {
		t.Fatalf("cancel must NOT change balance; got %d", w.BalanceMovement)
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
		`INSERT INTO token_wallets (id, master_id, balance_movement) VALUES ($1, $2, 50)`,
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
