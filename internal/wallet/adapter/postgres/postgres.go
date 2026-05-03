//go:build integration

// Package postgres is the production wallet repository, backed by pgx
// against the Postgres service in deploy/compose. Compiled only with
// the `integration` build tag so the default `go test ./...` and the
// CI lint stages do not require a database.
//
// Run integration tests with: `go test -tags integration ./internal/wallet/adapter/postgres/...`
//
// Connection string comes from the WALLET_PG_DSN env var; the test in
// postgres_test.go skips when unset.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/wallet"
)

// Repo is the pgx-backed wallet repository.
type Repo struct {
	pool *pgxpool.Pool
}

// New creates a Repo bound to pool. The caller owns the pool lifecycle.
func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// withTx runs fn inside a transaction; commits on nil error, rolls back
// otherwise. Returns whatever fn returned.
func (r *Repo) withTx(ctx context.Context, level pgx.TxIsoLevel, fn func(pgx.Tx) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: level})
	if err != nil {
		return fmt.Errorf("postgres: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Reserve implements port.Repository — F30 atomic reservation.
//
// Under SELECT ... FOR UPDATE on the wallet row, Reserve checks funds,
// inserts the pending ledger entry, and atomically applies the entry's
// signed amount to balance_movement inside the same TX. The balance
// write happens BEFORE the LLM call returns, which is the F30 invariant:
// two concurrent Reserves serialise on the row lock, the second sees
// the already-debited balance_movement, and the funds check correctly
// rejects an oversubscription. Production parity with memrepo.
func (r *Repo) Reserve(ctx context.Context, walletID string, entry wallet.LedgerEntry) (wallet.LedgerEntry, error) {
	if entry.ID == "" {
		entry.ID = uuid.NewString()
	}
	var out wallet.LedgerEntry
	err := r.withTx(ctx, pgx.ReadCommitted, func(tx pgx.Tx) error {
		var balance int64
		err := tx.QueryRow(ctx,
			`SELECT balance_movement FROM token_wallets WHERE id = $1 FOR UPDATE`,
			walletID,
		).Scan(&balance)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return wallet.ErrWalletNotFound
			}
			return fmt.Errorf("postgres: select wallet: %w", err)
		}
		if entry.Kind == wallet.KindDebit && balance < entry.Amount {
			return wallet.ErrInsufficientFunds
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO token_ledger (id, wallet_id, status, kind, source, amount, reference, created_at)
			VALUES ($1, $2, 'pending', $3, $4, $5, $6, $7)
		`, entry.ID, walletID, entry.Kind, entry.Source, entry.Amount, entry.Reference, entry.CreatedAt)
		if err != nil {
			return fmt.Errorf("postgres: insert ledger: %w", err)
		}
		// signed is positive for credit, negative for debit; adding it
		// to balance_movement applies the reservation directionally.
		signed := entry.SignedAmount()
		if _, err := tx.Exec(ctx,
			`UPDATE token_wallets SET balance_movement = balance_movement + $1, version = version + 1, updated_at = NOW() WHERE id = $2`,
			signed, walletID,
		); err != nil {
			return fmt.Errorf("postgres: apply reserve: %w", err)
		}
		entry.Status = wallet.StatusPending
		out = entry
		return nil
	})
	if err != nil {
		return wallet.LedgerEntry{}, err
	}
	return out, nil
}

// Commit implements port.Repository — pending → posted with no balance
// change. Reserve already debited balance_movement; Commit only flips
// status and bumps the wallet version. This is what makes the F37 retry
// loop safe: a second Commit on the same entry is a terminal
// ErrEntryAlreadyResolved, never a double-debit.
func (r *Repo) Commit(ctx context.Context, entryID string, postedAt time.Time) error {
	return r.withTx(ctx, pgx.ReadCommitted, func(tx pgx.Tx) error {
		var (
			walletID string
			status   string
		)
		err := tx.QueryRow(ctx,
			`SELECT wallet_id, status FROM token_ledger WHERE id = $1 FOR UPDATE`,
			entryID,
		).Scan(&walletID, &status)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return wallet.ErrEntryNotFound
			}
			return fmt.Errorf("postgres: select ledger: %w", err)
		}
		if status != string(wallet.StatusPending) {
			return wallet.ErrEntryAlreadyResolved
		}
		// Lock the wallet row so the version bump serialises with
		// concurrent Reserve/Cancel on the same wallet.
		if _, err := tx.Exec(ctx,
			`SELECT 1 FROM token_wallets WHERE id = $1 FOR UPDATE`,
			walletID,
		); err != nil {
			return fmt.Errorf("postgres: lock wallet: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE token_ledger SET status = 'posted', posted_at = $1 WHERE id = $2`,
			postedAt, entryID,
		); err != nil {
			return fmt.Errorf("postgres: post ledger: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE token_wallets SET version = version + 1, updated_at = $1 WHERE id = $2`,
			postedAt, walletID,
		); err != nil {
			return fmt.Errorf("postgres: bump wallet version: %w", err)
		}
		return nil
	})
}

// Cancel implements port.Repository — pending → cancelled and restores
// the reserved tokens to balance_movement so the rollback fully reverses
// the Reserve mutation. Wallet row is locked first so the restore
// composes atomically with concurrent Reserve attempts.
func (r *Repo) Cancel(ctx context.Context, entryID string, cancelledAt time.Time) error {
	return r.withTx(ctx, pgx.ReadCommitted, func(tx pgx.Tx) error {
		var (
			walletID string
			status   string
			kind     string
			amount   int64
		)
		err := tx.QueryRow(ctx,
			`SELECT wallet_id, status, kind, amount FROM token_ledger WHERE id = $1 FOR UPDATE`,
			entryID,
		).Scan(&walletID, &status, &kind, &amount)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return wallet.ErrEntryNotFound
			}
			return fmt.Errorf("postgres: select ledger: %w", err)
		}
		if status != string(wallet.StatusPending) {
			return wallet.ErrEntryAlreadyResolved
		}
		// Reverse the Reserve mutation: for a debit, add `amount` back;
		// for a credit reservation, subtract it.
		restore := amount
		if kind == string(wallet.KindCredit) {
			restore = -amount
		}
		if _, err := tx.Exec(ctx,
			`SELECT 1 FROM token_wallets WHERE id = $1 FOR UPDATE`,
			walletID,
		); err != nil {
			return fmt.Errorf("postgres: lock wallet: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE token_ledger SET status = 'cancelled', cancelled_at = $1 WHERE id = $2`,
			cancelledAt, entryID,
		); err != nil {
			return fmt.Errorf("postgres: cancel ledger: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE token_wallets SET balance_movement = balance_movement + $1, version = version + 1, updated_at = $2 WHERE id = $3`,
			restore, cancelledAt, walletID,
		); err != nil {
			return fmt.Errorf("postgres: restore balance: %w", err)
		}
		return nil
	})
}

// GetWallet implements port.Repository.
func (r *Repo) GetWallet(ctx context.Context, walletID string) (wallet.Wallet, error) {
	var w wallet.Wallet
	err := r.pool.QueryRow(ctx,
		`SELECT id, master_id, balance_movement, version, created_at, updated_at
		 FROM token_wallets WHERE id = $1`, walletID,
	).Scan(&w.ID, &w.MasterID, &w.BalanceMovement, &w.Version, &w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return wallet.Wallet{}, wallet.ErrWalletNotFound
		}
		return wallet.Wallet{}, fmt.Errorf("postgres: select wallet: %w", err)
	}
	return w, nil
}

// GetEntry implements port.Repository.
func (r *Repo) GetEntry(ctx context.Context, entryID string) (wallet.LedgerEntry, error) {
	var (
		e         wallet.LedgerEntry
		status    string
		kind      string
		source    string
		posted    *time.Time
		cancelled *time.Time
	)
	err := r.pool.QueryRow(ctx,
		`SELECT id, wallet_id, status, kind, source, amount, reference, attempts, created_at, posted_at, cancelled_at
		 FROM token_ledger WHERE id = $1`, entryID,
	).Scan(&e.ID, &e.WalletID, &status, &kind, &source, &e.Amount, &e.Reference, &e.Attempts, &e.CreatedAt, &posted, &cancelled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return wallet.LedgerEntry{}, wallet.ErrEntryNotFound
		}
		return wallet.LedgerEntry{}, fmt.Errorf("postgres: select ledger: %w", err)
	}
	e.Status = wallet.EntryStatus(status)
	e.Kind = wallet.EntryKind(kind)
	e.Source = wallet.EntrySource(source)
	e.PostedAt = posted
	e.CancelledAt = cancelled
	return e, nil
}

// ListWallets implements port.Repository.
func (r *Repo) ListWallets(ctx context.Context) ([]wallet.Wallet, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, master_id, balance_movement, version, created_at, updated_at FROM token_wallets ORDER BY id`,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: list wallets: %w", err)
	}
	defer rows.Close()
	out := make([]wallet.Wallet, 0)
	for rows.Next() {
		var w wallet.Wallet
		if err := rows.Scan(&w.ID, &w.MasterID, &w.BalanceMovement, &w.Version, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ListPendingOlderThan implements port.Repository.
func (r *Repo) ListPendingOlderThan(ctx context.Context, before time.Time) ([]wallet.LedgerEntry, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, wallet_id, status, kind, source, amount, reference, attempts, created_at
		 FROM token_ledger WHERE status = 'pending' AND created_at < $1 ORDER BY id`, before,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: list pending: %w", err)
	}
	defer rows.Close()
	out := make([]wallet.LedgerEntry, 0)
	for rows.Next() {
		var (
			e      wallet.LedgerEntry
			status string
			kind   string
			source string
		)
		if err := rows.Scan(&e.ID, &e.WalletID, &status, &kind, &source, &e.Amount, &e.Reference, &e.Attempts, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Status = wallet.EntryStatus(status)
		e.Kind = wallet.EntryKind(kind)
		e.Source = wallet.EntrySource(source)
		out = append(out, e)
	}
	return out, rows.Err()
}

// SumPostedByWallet implements port.Repository.
func (r *Repo) SumPostedByWallet(ctx context.Context, walletID string) (int64, error) {
	var sum int64
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN kind = 'credit' THEN amount ELSE -amount END), 0)
		FROM token_ledger WHERE wallet_id = $1 AND status = 'posted'
	`, walletID).Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("postgres: sum posted: %w", err)
	}
	return sum, nil
}

// CountPending implements port.Repository.
func (r *Repo) CountPending(ctx context.Context) (int64, error) {
	var n int64
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM token_ledger WHERE status = 'pending'`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("postgres: count pending: %w", err)
	}
	return n, nil
}

// IncrementAttempts implements port.Repository.
func (r *Repo) IncrementAttempts(ctx context.Context, entryID string) error {
	ct, err := r.pool.Exec(ctx, `UPDATE token_ledger SET attempts = attempts + 1 WHERE id = $1`, entryID)
	if err != nil {
		return fmt.Errorf("postgres: bump attempts: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return wallet.ErrEntryNotFound
	}
	return nil
}
