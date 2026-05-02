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

// Reserve implements port.Repository under SELECT ... FOR UPDATE.
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
		_, err = tx.Exec(ctx,
			`UPDATE token_wallets SET version = version + 1, updated_at = NOW() WHERE id = $1`,
			walletID,
		)
		if err != nil {
			return fmt.Errorf("postgres: bump wallet version: %w", err)
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

// Commit implements port.Repository.
func (r *Repo) Commit(ctx context.Context, entryID string, postedAt time.Time) error {
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
		signed := amount
		if kind == string(wallet.KindDebit) {
			signed = -amount
		}
		if _, err := tx.Exec(ctx,
			`UPDATE token_wallets SET balance_movement = balance_movement + $1, version = version + 1, updated_at = $2 WHERE id = $3`,
			signed, postedAt, walletID,
		); err != nil {
			return fmt.Errorf("postgres: apply balance: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE token_ledger SET status = 'posted', posted_at = $1 WHERE id = $2`,
			postedAt, entryID,
		); err != nil {
			return fmt.Errorf("postgres: post ledger: %w", err)
		}
		return nil
	})
}

// Cancel implements port.Repository.
func (r *Repo) Cancel(ctx context.Context, entryID string, cancelledAt time.Time) error {
	return r.withTx(ctx, pgx.ReadCommitted, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE token_ledger SET status = 'cancelled', cancelled_at = $1 WHERE id = $2 AND status = 'pending'`,
			cancelledAt, entryID,
		)
		if err != nil {
			return fmt.Errorf("postgres: cancel ledger: %w", err)
		}
		if ct.RowsAffected() == 0 {
			// Distinguish missing vs already resolved.
			var status string
			scanErr := tx.QueryRow(ctx, `SELECT status FROM token_ledger WHERE id = $1`, entryID).Scan(&status)
			if scanErr != nil {
				return wallet.ErrEntryNotFound
			}
			return wallet.ErrEntryAlreadyResolved
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
