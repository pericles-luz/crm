package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pericles-luz/crm/internal/webhook"
)

// RawEventStore implements webhook.RawEventStore against the partitioned
// raw_event table. Headers are written as JSONB; payload as bytea.
type RawEventStore struct {
	db PgxConn
}

// NewRawEventStore returns a store bound to the given pgx pool/conn.
func NewRawEventStore(db PgxConn) *RawEventStore { return &RawEventStore{db: db} }

const rawEventInsertSQL = `
INSERT INTO raw_event
  (tenant_id, channel, idempotency_key, raw_payload, headers, received_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id
`

// Insert appends a row and returns the generated event id. Headers are
// JSON-encoded at the boundary so the domain stays free of jsonb types.
func (s *RawEventStore) Insert(ctx context.Context, row webhook.RawEventRow) ([16]byte, error) {
	headersJSON, err := json.Marshal(row.Headers)
	if err != nil {
		return [16]byte{}, fmt.Errorf("marshal headers: %w", err)
	}
	var id [16]byte
	if err := s.db.QueryRow(ctx, rawEventInsertSQL,
		row.TenantID[:],
		row.Channel,
		row.IdempotencyKey,
		row.Payload,
		headersJSON,
		row.ReceivedAt,
	).Scan(&id); err != nil {
		return [16]byte{}, fmt.Errorf("raw_event insert: %w", err)
	}
	return id, nil
}

const rawEventMarkPublishedSQL = `
UPDATE raw_event
   SET published_at = $2
 WHERE id = $1
`

// MarkPublished records that the event has been ack'd by the broker.
// The reconciler (ADR §2 D7) re-tries any rows where this never lands.
func (s *RawEventStore) MarkPublished(ctx context.Context, eventID [16]byte, now time.Time) error {
	_, err := s.db.Exec(ctx, rawEventMarkPublishedSQL, eventID[:], now)
	if err != nil {
		return fmt.Errorf("raw_event mark published: %w", err)
	}
	return nil
}
