package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/wallet/adapter/memrepo"
	"github.com/pericles-luz/crm/internal/wallet/usecase"
)

// TestReserve_HappyPath asserts AC #1: a Reserve creates a pending
// ledger entry without changing balance_movement.
func TestReserve_HappyPath(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	repo := memrepo.New()
	repo.Now = clock.Now
	repo.SeedWallet(wallet.Wallet{ID: "w1", MasterID: "m1", BalanceMovement: 1000})

	r := usecase.Reserve{Repo: repo, IDs: &seqIDs{}, Clock: clock}
	entry, err := r.Run(ctx, usecase.ReserveInput{
		WalletID: "w1", Amount: 100, Source: wallet.SourceLLMCall, Reference: "req-1",
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if entry.Status != wallet.StatusPending {
		t.Fatalf("status: got %s, want pending", entry.Status)
	}
	if entry.Amount != 100 || entry.Kind != wallet.KindDebit {
		t.Fatalf("entry shape unexpected: %+v", entry)
	}
	w, _ := repo.GetWallet(ctx, "w1")
	if w.BalanceMovement != 900 {
		t.Fatalf("F30 atomic reserve must subtract amount; got balance %d, want 900", w.BalanceMovement)
	}
}

// TestReserve_InsufficientFunds asserts F30 atomic guard surfaces ErrInsufficientFunds.
func TestReserve_InsufficientFunds(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(time.Now())
	repo := memrepo.New()
	repo.SeedWallet(wallet.Wallet{ID: "w2", MasterID: "m1", BalanceMovement: 50})
	r := usecase.Reserve{Repo: repo, IDs: &seqIDs{}, Clock: clock}
	_, err := r.Run(ctx, usecase.ReserveInput{
		WalletID: "w2", Amount: 100, Source: wallet.SourceLLMCall,
	})
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("got %v, want ErrInsufficientFunds", err)
	}
}

// TestReserve_InvalidInput covers branch coverage for malformed input.
func TestReserve_InvalidInput(t *testing.T) {
	ctx := context.Background()
	repo := memrepo.New()
	r := usecase.Reserve{Repo: repo, IDs: &seqIDs{}, Clock: newFakeClock(time.Now())}

	if _, err := r.Run(ctx, usecase.ReserveInput{Amount: 0, WalletID: "w"}); !errors.Is(err, wallet.ErrInvalidAmount) {
		t.Fatalf("zero amount: got %v", err)
	}
	if _, err := r.Run(ctx, usecase.ReserveInput{Amount: 1, WalletID: ""}); !errors.Is(err, wallet.ErrWalletNotFound) {
		t.Fatalf("empty wallet id: got %v", err)
	}
	// missing wallet → repository returns ErrWalletNotFound (domain
	// passthrough not wrapped as transient).
	if _, err := r.Run(ctx, usecase.ReserveInput{Amount: 1, WalletID: "nope", Source: wallet.SourceLLMCall}); !errors.Is(err, wallet.ErrWalletNotFound) {
		t.Fatalf("missing wallet: got %v", err)
	}
}

// TestReserve_TransientWrap proves an unrelated repo error is wrapped as
// ErrTransient so callers can branch on it.
func TestReserve_TransientWrap(t *testing.T) {
	ctx := context.Background()
	repo := memrepo.New()
	repo.SeedWallet(wallet.Wallet{ID: "w3", MasterID: "m1", BalanceMovement: 1000})
	repo.FailNext()
	r := usecase.Reserve{Repo: repo, IDs: &seqIDs{}, Clock: newFakeClock(time.Now())}
	_, err := r.Run(ctx, usecase.ReserveInput{WalletID: "w3", Amount: 1, Source: wallet.SourceLLMCall})
	if !errors.Is(err, wallet.ErrTransient) {
		t.Fatalf("got %v, want ErrTransient", err)
	}
}
