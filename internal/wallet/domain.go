// Package wallet holds the domain aggregate and value objects for the
// SIN token wallet (F30 reserve atômico + F37 commit-after-LLM resilience).
//
// Domain code MUST NOT import database/sql, net/http, or vendor SDKs;
// every effect goes through a port in the port subpackage.
package wallet

import (
	"errors"
	"time"
)

// EntryStatus is the lifecycle stage of a ledger entry.
type EntryStatus string

const (
	StatusPending   EntryStatus = "pending"
	StatusPosted    EntryStatus = "posted"
	StatusCancelled EntryStatus = "cancelled"
)

// EntryKind tells whether the entry adds or removes balance.
type EntryKind string

const (
	KindDebit  EntryKind = "debit"
	KindCredit EntryKind = "credit"
)

// EntrySource is the producer of the ledger entry.
type EntrySource string

const (
	SourceLLMCall        EntrySource = "llm_call"
	SourceReconciliation EntrySource = "reconciliation"
	SourceGrant          EntrySource = "grant"
	SourceRefund         EntrySource = "refund"
)

// signedAmount returns the entry amount with sign applied for kind.
// Used by reconciliators that fold the ledger into a single number.
func (e LedgerEntry) signedAmount() int64 {
	if e.Kind == KindCredit {
		return e.Amount
	}
	return -e.Amount
}

// Wallet is the aggregate root.
//
// BalanceMovement is the live "available tokens" balance. Reserve
// reduces it atomically (so concurrent reserves cannot oversubscribe);
// Commit is a no-op on BalanceMovement; Cancel restores it.
//
// InitialBalance is the genesis grant captured at wallet creation; it
// never changes. The nightly drift check compares
//
//	|sum_posted_signed + (InitialBalance - BalanceMovement)|
//
// against InitialBalance, so a stuck pending entry shows up as drift
// while a successful commit balances out to ~0.
type Wallet struct {
	ID              string
	MasterID        string
	InitialBalance  int64
	BalanceMovement int64
	Version         int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// LedgerEntry is the append-only record of any movement on a wallet.
// The balance is reconstructable by folding posted entries.
type LedgerEntry struct {
	ID          string
	WalletID    string
	Status      EntryStatus
	Kind        EntryKind
	Source      EntrySource
	Amount      int64 // strictly positive; sign comes from Kind
	Reference   string
	CreatedAt   time.Time
	PostedAt    *time.Time
	CancelledAt *time.Time
	Attempts    int32 // increments on retried commit attempts
}

// Validate enforces the invariants that domain operations rely on.
func (e LedgerEntry) Validate() error {
	if e.Amount <= 0 {
		return ErrInvalidAmount
	}
	switch e.Kind {
	case KindDebit, KindCredit:
	default:
		return ErrInvalidKind
	}
	switch e.Source {
	case SourceLLMCall, SourceReconciliation, SourceGrant, SourceRefund:
	default:
		return ErrInvalidSource
	}
	switch e.Status {
	case StatusPending, StatusPosted, StatusCancelled:
	default:
		return ErrInvalidStatus
	}
	return nil
}

// IsPending tells whether an entry is awaiting commit/cancel.
func (e LedgerEntry) IsPending() bool { return e.Status == StatusPending }

// SignedAmount exposes the signed-by-kind amount. Used by reconcilers
// that fold posted entries into the ledger sum.
func (e LedgerEntry) SignedAmount() int64 { return e.signedAmount() }

// Aggregate sentinel errors. Use errors.Is to test.
var (
	ErrWalletNotFound       = errors.New("wallet: wallet not found")
	ErrEntryNotFound        = errors.New("wallet: ledger entry not found")
	ErrInsufficientFunds    = errors.New("wallet: insufficient funds for reservation")
	ErrEntryAlreadyResolved = errors.New("wallet: entry already posted or cancelled")
	ErrInvalidAmount        = errors.New("wallet: amount must be positive")
	ErrInvalidKind          = errors.New("wallet: invalid entry kind")
	ErrInvalidSource        = errors.New("wallet: invalid entry source")
	ErrInvalidStatus        = errors.New("wallet: invalid entry status")
	ErrCommitExhausted      = errors.New("wallet: commit retries exhausted; queued for reconciliation")
	ErrTransient            = errors.New("wallet: transient adapter failure")
)
