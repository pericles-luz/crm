// Package port declares the interfaces the wallet domain relies on.
// Concrete implementations live under internal/wallet/adapter/...
package port

import (
	"context"
	"time"

	"github.com/pericles-luz/crm/internal/wallet"
)

// Repository is the storage port for wallets and ledger entries.
//
// Implementations MUST execute Reserve and Commit transactionally so
// that wallet row + ledger row mutate atomically (e.g. SELECT FOR UPDATE
// on the wallet row in pgx, single-mutex on the in-memory adapter).
//
// Reserve validates funds, atomically subtracts the debit amount from
// balance_movement, and inserts a pending ledger entry. Commit converts
// pending → posted without touching balance_movement (it was already
// reduced at reserve). Cancel converts pending → cancelled and restores
// the reserved amount to balance_movement.
type Repository interface {
	// Reserve atomically locks the wallet, checks funds, subtracts the
	// reserved amount from balance_movement, and inserts a pending
	// ledger entry. Returns the persisted entry (with ID).
	// MUST return wallet.ErrInsufficientFunds when balance is too low.
	Reserve(ctx context.Context, walletID string, entry wallet.LedgerEntry) (wallet.LedgerEntry, error)

	// Commit converts a pending entry to posted. Balance_movement was
	// already adjusted by Reserve so Commit is a status-only flip.
	// MUST return wallet.ErrEntryAlreadyResolved if the entry is not pending.
	Commit(ctx context.Context, entryID string, postedAt time.Time) error

	// Cancel converts a pending entry to cancelled and restores the
	// reserved tokens to balance_movement.
	Cancel(ctx context.Context, entryID string, cancelledAt time.Time) error

	// GetWallet returns a wallet by id.
	GetWallet(ctx context.Context, walletID string) (wallet.Wallet, error)

	// GetEntry returns a ledger entry by id.
	GetEntry(ctx context.Context, entryID string) (wallet.LedgerEntry, error)

	// ListWallets returns all wallets (used by reconciliator).
	// Implementations MAY page; reconciliator is happy with full lists
	// for the small wallet count we expect in fase 1.
	ListWallets(ctx context.Context) ([]wallet.Wallet, error)

	// ListPendingOlderThan returns pending entries created before "before".
	// Used by the reconciliator to rescue stuck reservations.
	ListPendingOlderThan(ctx context.Context, before time.Time) ([]wallet.LedgerEntry, error)

	// SumPostedByWallet returns the signed sum of posted entries for the
	// wallet (folding kind into sign). Used by drift detection.
	SumPostedByWallet(ctx context.Context, walletID string) (int64, error)

	// CountPending returns the count of pending entries across all wallets.
	// Used to update wallet_pending_entries_gauge.
	CountPending(ctx context.Context) (int64, error)

	// IncrementAttempts records that a commit attempt happened on the
	// entry without changing its status. Used by retry logic for audit.
	IncrementAttempts(ctx context.Context, entryID string) error
}
