// Package contactbackfill is the cross-tenant read/write side of the
// one-shot "backfill real contact names" maintenance tool
// (cmd/backfill-contact-names). It is not part of the runtime request
// path — no server wire references this package.
//
// Every contact created before the messenger/instagram profile-fetch
// fix (see internal/adapter/channel/{messenger,instagram}/profile.go)
// got the raw PSID/IGSID stored as its display_name, because
// contacts.New requires a non-empty name and no real name was available
// at the time. That state is exactly, and only, identifiable by
// contact.display_name == contact_channel_identity.external_id — no
// heuristic needed, since a real person's name being byte-identical to
// a numeric platform id never happens.
package contactbackfill

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
)

// backfillActorSentinel is the actor uuid this tool's writes are
// attributed to in master_ops_audit — the trigger requires a non-nil
// actor; this sentinel makes the tool's writes greppable, mirroring
// internal/adapter/db/postgres/lgpd/store.go's masterActorSentinel.
var backfillActorSentinel = uuid.MustParse("00000000-0000-0000-0000-000000000064")

// Store is the cross-tenant adapter this tool needs: it reads and
// writes through postgres.WithMasterOps (not WithTenant), the same
// mechanism cmd/lgpd-retention-purge-worker uses for the same shape of
// problem — a background job that must see and touch every tenant. The
// contact / contact_channel_identity tables already grant app_master_ops
// full SELECT/INSERT/UPDATE/DELETE (migration 0088_inbox_contacts), and
// every write here lands in master_ops_audit automatically via the
// contact_master_ops_audit trigger.
type Store struct {
	masterPool *pgxpool.Pool
}

// New binds a Store to pool, which MUST be connected as app_master_ops
// (MASTER_OPS_DATABASE_URL in production — the same env var every other
// cmd/server master-ops consumer reads; cmd/lgpd-retention-purge-worker
// is the one outlier that reads DATABASE_MASTER_OPS_URL instead, which
// this tool deliberately does not follow).
func New(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, errors.New("contactbackfill: pool is nil")
	}
	return &Store{masterPool: pool}, nil
}

// Candidate is one contact whose display_name still equals its raw
// channel external_id — a name this tool can try to resolve.
type Candidate struct {
	ContactID  uuid.UUID
	TenantID   uuid.UUID
	Channel    string // "messenger" | "instagram"
	ExternalID string // the raw PSID/IGSID, == the contact's current display_name
}

// ListCandidates scans every tenant for contacts that never got a real
// name. channel narrows to "messenger" or "instagram"; empty means
// both. Results are ordered by tenant then contact id for a stable,
// resumable scan.
func (s *Store) ListCandidates(ctx context.Context, channel string) ([]Candidate, error) {
	const baseSQL = `
		SELECT c.id, c.tenant_id, cci.channel, cci.external_id
		  FROM contact c
		  JOIN contact_channel_identity cci
		    ON cci.contact_id = c.id AND cci.tenant_id = c.tenant_id
		 WHERE cci.channel IN ('messenger', 'instagram')
		   AND c.display_name = cci.external_id`
	sql := baseSQL + " ORDER BY c.tenant_id, c.id"
	args := []any{}
	if channel != "" {
		sql = baseSQL + " AND cci.channel = $1 ORDER BY c.tenant_id, c.id"
		args = append(args, channel)
	}

	var out []Candidate
	err := postgres.WithMasterOps(ctx, s.masterPool, backfillActorSentinel, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return fmt.Errorf("list candidates: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var c Candidate
			if err := rows.Scan(&c.ContactID, &c.TenantID, &c.Channel, &c.ExternalID); err != nil {
				return fmt.Errorf("scan candidate: %w", err)
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateDisplayName sets contactID's display_name to newName, guarded on
// display_name still equalling oldExternalID. When the guard doesn't
// match — an operator renamed the contact between ListCandidates and
// this call — the update affects 0 rows and updated is false with a nil
// error; the caller should treat that as "skipped", not a failure.
func (s *Store) UpdateDisplayName(ctx context.Context, tenantID, contactID uuid.UUID, oldExternalID, newName string) (bool, error) {
	var updated bool
	err := postgres.WithMasterOps(ctx, s.masterPool, backfillActorSentinel, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE contact
			   SET display_name = $1, updated_at = now()
			 WHERE id = $2 AND tenant_id = $3 AND display_name = $4
		`, newName, contactID, tenantID, oldExternalID)
		if err != nil {
			return fmt.Errorf("update display name: %w", err)
		}
		updated = tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return updated, nil
}
