// Package postgres implements webhook.TokenStore, webhook.IdempotencyStore,
// and webhook.RawEventStore against a real Postgres instance via pgx.
//
// The lookups in this file rely on the partial unique index
// `webhook_tokens_active_idx (channel, token_hash) WHERE revoked_at IS NULL`
// declared in 0075a. Filtering by token_hash uses bytea equality (constant
// in the SQL planner) and the unique index — that combination is the
// timing-attack-safe lookup the ADR §3 promises.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pericles-luz/crm/internal/webhook"
)

// PgxConn is the narrowest pgx.Conn surface this package needs. Using an
// interface keeps the adapter unit-testable without mocking the driver
// (the integration tests use a real *pgxpool.Pool that satisfies it).
type PgxConn interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// TokenStore implements webhook.TokenStore against Postgres.
type TokenStore struct {
	db PgxConn
}

// NewTokenStore returns a TokenStore bound to the given pgx pool/conn.
func NewTokenStore(db PgxConn) *TokenStore { return &TokenStore{db: db} }

// rev 3 / F-13 — `revoked_at` is the scheduled effective revocation
// timestamp, NOT "is revoked from now". A token is valid while
// revoked_at IS NULL (permanently active) or now() < revoked_at (in the
// rotation grace window). The query selects the most recent row first
// so that "active forever" rows beat overlapping grace rows when both
// share the same hash (collisions are astronomical, but the precedence
// is well-defined).
const tokenLookupSQL = `
SELECT tenant_id, revoked_at
FROM webhook_tokens
WHERE channel = $1 AND token_hash = $2
ORDER BY (revoked_at IS NULL) DESC, created_at DESC
LIMIT 1
`

// Lookup implements webhook.TokenStore. Returns ErrTokenUnknown when
// the (channel, token_hash) pair has no row and ErrTokenRevoked when
// the most-recent row's revoked_at has already elapsed. The grace
// window is encoded in revoked_at itself (rotation sets it to
// `now() + overlap_minutes`); this layer just compares against `now`.
func (s *TokenStore) Lookup(ctx context.Context, channel string, tokenHash []byte, now time.Time) (webhook.TenantID, error) {
	var (
		raw     [16]byte
		revoked *time.Time
	)
	row := s.db.QueryRow(ctx, tokenLookupSQL, channel, tokenHash)
	if err := row.Scan(&raw, &revoked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return webhook.TenantID{}, webhook.ErrTokenUnknown
		}
		return webhook.TenantID{}, fmt.Errorf("token lookup: %w", err)
	}
	if revoked != nil && !now.Before(*revoked) {
		return webhook.TenantID{}, webhook.ErrTokenRevoked
	}
	return webhook.TenantID(raw), nil
}

const tokenMarkUsedSQL = `
UPDATE webhook_tokens
   SET last_used_at = $3
 WHERE channel = $1 AND token_hash = $2
`

// MarkUsed bumps last_used_at; failures are best-effort (the caller in
// service.go ignores the error).
func (s *TokenStore) MarkUsed(ctx context.Context, channel string, tokenHash []byte, now time.Time) error {
	_, err := s.db.Exec(ctx, tokenMarkUsedSQL, channel, tokenHash, now)
	if err != nil {
		return fmt.Errorf("mark used: %w", err)
	}
	return nil
}
