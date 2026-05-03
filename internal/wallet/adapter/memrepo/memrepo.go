// Package memrepo is an in-memory wallet repository that matches the
// transactional semantics of the production pgx adapter — single-mutex
// per repo serialises Reserve/Commit/Cancel, balance_movement updates
// atomically with ledger transitions, and pending entries are
// rejected on a second commit.
//
// This is NOT a mock: tests interact with this adapter exactly as
// production code would, and the unit tests in internal/wallet/...
// rely on its real transactional behaviour. See the CTO rule:
// "no mocking the database in tests for code that actually touches
// storage. Use the real test DB or a documented in-memory adapter
// that matches production behaviour."
package memrepo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pericles-luz/crm/internal/wallet"
)

// Repo is an in-memory implementation of port.Repository.
type Repo struct {
	mu       sync.Mutex
	wallets  map[string]wallet.Wallet
	entries  map[string]wallet.LedgerEntry
	nextID   atomic.Int64

	// Failure injection — tests use these to simulate DB outages.
	// FailOnce makes the next non-read mutation return ErrInjected, then resets.
	failOnce atomic.Bool
	// FailUntil makes every non-read mutation fail until the clock passes
	// the deadline; used to simulate "DB unavailable for 2 seconds".
	failUntilNanos atomic.Int64
	// PermanentFail makes every non-read mutation fail forever.
	permanentFail atomic.Bool

	// Now is consulted by the failure window so tests don't need to
	// inject a real clock; defaults to time.Now.
	Now func() time.Time
}

// ErrInjected is the sentinel returned when failure injection fires.
// Treated as transient by the use cases (so retries kick in).
var ErrInjected = errors.New("memrepo: injected failure")

// New creates an empty repo.
func New() *Repo {
	return &Repo{
		wallets: map[string]wallet.Wallet{},
		entries: map[string]wallet.LedgerEntry{},
	}
}

// SeedWallet inserts a wallet for tests. If InitialBalance is unset
// it defaults to BalanceMovement (matching genesis-grant semantics).
func (r *Repo) SeedWallet(w wallet.Wallet) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w.CreatedAt.IsZero() {
		w.CreatedAt = r.now()
	}
	w.UpdatedAt = w.CreatedAt
	if w.InitialBalance == 0 {
		w.InitialBalance = w.BalanceMovement
	}
	r.wallets[w.ID] = w
}

// DeleteWallet removes a wallet from the in-memory store. It is a test
// helper for simulating wallet churn — the production pgx adapter
// performs deletes through the wallet domain operations, but the
// reconciliator only sees a wallet via ListWallets so a direct removal
// here matches the "wallet fell out of ListWallets" condition exercised
// by SIN-62269.
func (r *Repo) DeleteWallet(walletID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.wallets, walletID)
}

// FailNext makes the next mutating call return ErrInjected.
func (r *Repo) FailNext() { r.failOnce.Store(true) }

// FailFor makes mutating calls return ErrInjected for the given window
// starting now (according to the repo clock).
func (r *Repo) FailFor(d time.Duration) {
	r.failUntilNanos.Store(r.now().Add(d).UnixNano())
}

// FailPermanently makes all mutating calls return ErrInjected until
// reset. Combined with the worker escalation threshold, this lets us
// drive the AC #7 "DB indisponível persistente" scenario.
func (r *Repo) FailPermanently(b bool) { r.permanentFail.Store(b) }

func (r *Repo) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Repo) shouldFail() bool {
	if r.permanentFail.Load() {
		return true
	}
	if r.failOnce.Swap(false) {
		return true
	}
	until := r.failUntilNanos.Load()
	if until > 0 && r.now().UnixNano() < until {
		return true
	}
	return false
}

// nextEntryID returns a deterministic id used when callers don't pass one.
func (r *Repo) nextEntryID() string {
	return fmt.Sprintf("entry-%d", r.nextID.Add(1))
}

// Reserve implements port.Repository — F30 atomic reservation.
//
// Under the repo mutex (== "SELECT ... FOR UPDATE" in the production
// pgx adapter), Reserve checks whether the wallet has enough live
// balance, rejects with ErrInsufficientFunds if not, and otherwise
// atomically subtracts the debit amount from BalanceMovement and
// inserts a pending ledger entry. The ledger entry preserves the full
// audit; BalanceMovement reflects "available tokens right now".
func (r *Repo) Reserve(ctx context.Context, walletID string, entry wallet.LedgerEntry) (wallet.LedgerEntry, error) {
	if err := ctx.Err(); err != nil {
		return wallet.LedgerEntry{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shouldFail() {
		return wallet.LedgerEntry{}, ErrInjected
	}
	w, ok := r.wallets[walletID]
	if !ok {
		return wallet.LedgerEntry{}, wallet.ErrWalletNotFound
	}
	if entry.Kind == wallet.KindDebit && w.BalanceMovement < entry.Amount {
		return wallet.LedgerEntry{}, wallet.ErrInsufficientFunds
	}
	if entry.ID == "" {
		entry.ID = r.nextEntryID()
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = r.now()
	}
	entry.Status = wallet.StatusPending
	r.entries[entry.ID] = entry
	// Atomic balance update is what makes F30 race-safe.
	w.BalanceMovement += entry.SignedAmount()
	w.Version++
	w.UpdatedAt = r.now()
	r.wallets[walletID] = w
	return entry, nil
}

// Commit implements port.Repository — F37 commit-after-LLM.
//
// Commit only flips status pending → posted and records posted_at.
// BalanceMovement is unchanged because Reserve already debited it; if
// we double-debited here, persistent retries (which are exactly the
// resilience scenario this is here to handle) would corrupt the wallet.
func (r *Repo) Commit(ctx context.Context, entryID string, postedAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shouldFail() {
		return ErrInjected
	}
	e, ok := r.entries[entryID]
	if !ok {
		return wallet.ErrEntryNotFound
	}
	if e.Status != wallet.StatusPending {
		return wallet.ErrEntryAlreadyResolved
	}
	w, ok := r.wallets[e.WalletID]
	if !ok {
		return wallet.ErrWalletNotFound
	}
	e.Status = wallet.StatusPosted
	pa := postedAt
	e.PostedAt = &pa
	r.entries[entryID] = e
	w.Version++
	w.UpdatedAt = postedAt
	r.wallets[e.WalletID] = w
	return nil
}

// Cancel implements port.Repository.
//
// Cancel restores the reserved amount to BalanceMovement so the
// rollback fully reverses the Reserve. Pending → cancelled is the
// only allowed transition; double-cancel is terminal.
func (r *Repo) Cancel(ctx context.Context, entryID string, cancelledAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shouldFail() {
		return ErrInjected
	}
	e, ok := r.entries[entryID]
	if !ok {
		return wallet.ErrEntryNotFound
	}
	if e.Status != wallet.StatusPending {
		return wallet.ErrEntryAlreadyResolved
	}
	w, ok := r.wallets[e.WalletID]
	if !ok {
		return wallet.ErrWalletNotFound
	}
	e.Status = wallet.StatusCancelled
	ca := cancelledAt
	e.CancelledAt = &ca
	r.entries[entryID] = e
	// Restore the reserved tokens.
	w.BalanceMovement -= e.SignedAmount()
	w.Version++
	w.UpdatedAt = cancelledAt
	r.wallets[e.WalletID] = w
	return nil
}

// GetWallet implements port.Repository.
func (r *Repo) GetWallet(ctx context.Context, walletID string) (wallet.Wallet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.wallets[walletID]
	if !ok {
		return wallet.Wallet{}, wallet.ErrWalletNotFound
	}
	return w, nil
}

// GetEntry implements port.Repository.
func (r *Repo) GetEntry(ctx context.Context, entryID string) (wallet.LedgerEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[entryID]
	if !ok {
		return wallet.LedgerEntry{}, wallet.ErrEntryNotFound
	}
	return e, nil
}

// ListWallets implements port.Repository.
func (r *Repo) ListWallets(ctx context.Context) ([]wallet.Wallet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]wallet.Wallet, 0, len(r.wallets))
	for _, w := range r.wallets {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ListPendingOlderThan implements port.Repository.
func (r *Repo) ListPendingOlderThan(ctx context.Context, before time.Time) ([]wallet.LedgerEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]wallet.LedgerEntry, 0)
	for _, e := range r.entries {
		if e.Status == wallet.StatusPending && e.CreatedAt.Before(before) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// SumPostedByWallet implements port.Repository.
func (r *Repo) SumPostedByWallet(ctx context.Context, walletID string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var sum int64
	for _, e := range r.entries {
		if e.WalletID == walletID && e.Status == wallet.StatusPosted {
			sum += e.SignedAmount()
		}
	}
	return sum, nil
}

// CountPending implements port.Repository.
func (r *Repo) CountPending(ctx context.Context) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for _, e := range r.entries {
		if e.Status == wallet.StatusPending {
			n++
		}
	}
	return n, nil
}

// IncrementAttempts implements port.Repository.
func (r *Repo) IncrementAttempts(ctx context.Context, entryID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[entryID]
	if !ok {
		return wallet.ErrEntryNotFound
	}
	e.Attempts++
	r.entries[entryID] = e
	return nil
}
