package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pericles-luz/crm/internal/webhook"
)

// IdempotencyStore implements webhook.IdempotencyStore against Postgres.
//
// The composite primary key (tenant_id, channel, idempotency_key) is the
// dedup invariant. INSERT … ON CONFLICT DO NOTHING is the boring,
// race-free way to either claim the slot or detect a replay; we use
// RETURNING to disambiguate the two outcomes without a second roundtrip.
type IdempotencyStore struct {
	db PgxConn
}

// NewIdempotencyStore returns a store bound to the given pgx pool/conn.
func NewIdempotencyStore(db PgxConn) *IdempotencyStore { return &IdempotencyStore{db: db} }

const idemInsertSQL = `
INSERT INTO webhook_idempotency (tenant_id, channel, idempotency_key, inserted_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_id, channel, idempotency_key) DO NOTHING
RETURNING idempotency_key
`

// CheckAndStore implements webhook.IdempotencyStore. firstSeen is true
// iff the row was newly inserted; false means the conflict path fired
// (replay).
func (s *IdempotencyStore) CheckAndStore(
	ctx context.Context,
	tenantID webhook.TenantID,
	channel string,
	key []byte,
	now time.Time,
) (bool, error) {
	row := s.db.QueryRow(ctx, idemInsertSQL, tenantID[:], channel, key, now)
	var stored []byte
	switch err := row.Scan(&stored); {
	case err == nil:
		return true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("idempotency insert: %w", err)
	}
}
