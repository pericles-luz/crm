// Package usecase wires the wallet domain (F30 reserve atômico,
// F37 commit-after-LLM resilience) through ports.
package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/wallet/port"
)

// Reserve is F30 — the atomic "reserve before LLM call" use case.
//
// Reserve does the following under a single repository transaction:
//   1. SELECT FOR UPDATE on the wallet row.
//   2. Validate balance_movement >= amount for debit.
//   3. Insert a pending ledger entry.
//   4. Commit the transaction.
//
// The returned LedgerEntry holds the reservation id; callers pass that
// id to CommitDebit after the LLM call returns OK, or to CancelDebit on
// rollback.
type Reserve struct {
	Repo  port.Repository
	IDs   port.IDGenerator
	Clock port.Clock
}

// ReserveInput captures the parameters of a single reservation.
type ReserveInput struct {
	WalletID  string
	Amount    int64
	Source    wallet.EntrySource
	Reference string
}

// Run reserves Amount tokens against WalletID and returns the persisted
// pending ledger entry.
func (r Reserve) Run(ctx context.Context, in ReserveInput) (wallet.LedgerEntry, error) {
	if in.Amount <= 0 {
		return wallet.LedgerEntry{}, wallet.ErrInvalidAmount
	}
	if in.WalletID == "" {
		return wallet.LedgerEntry{}, wallet.ErrWalletNotFound
	}
	now := r.Clock.Now()
	entry := wallet.LedgerEntry{
		ID:        r.IDs.NewID(),
		WalletID:  in.WalletID,
		Status:    wallet.StatusPending,
		Kind:      wallet.KindDebit,
		Source:    in.Source,
		Amount:    in.Amount,
		Reference: in.Reference,
		CreatedAt: now,
	}
	if err := entry.Validate(); err != nil {
		return wallet.LedgerEntry{}, err
	}
	persisted, err := r.Repo.Reserve(ctx, in.WalletID, entry)
	if err != nil {
		// Domain-level errors pass through unchanged so callers can
		// errors.Is them. Anything else gets wrapped as transient so
		// the caller knows it was an adapter failure.
		if errors.Is(err, wallet.ErrInsufficientFunds) ||
			errors.Is(err, wallet.ErrWalletNotFound) ||
			errors.Is(err, wallet.ErrInvalidAmount) {
			return wallet.LedgerEntry{}, err
		}
		return wallet.LedgerEntry{}, fmt.Errorf("wallet/reserve: %w: %w", wallet.ErrTransient, err)
	}
	return persisted, nil
}
