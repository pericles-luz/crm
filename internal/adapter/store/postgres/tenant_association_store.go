package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pericles-luz/crm/internal/webhook"
)

// TenantAssociationStore implements webhook.TenantAssociationStore
// (rev 3 / F-12) against the tenant_channel_associations table from
// migration 0075a2.
//
// The query is a one-shot scalar EXISTS so the planner uses the
// (channel, association) primary key. We never return whether the
// (channel, association) row existed at all under a different tenant —
// that distinction is intentionally invisible to callers per the port
// contract (avoid leaking association presence).
type TenantAssociationStore struct {
	db PgxConn
}

// NewTenantAssociationStore returns a store bound to db.
func NewTenantAssociationStore(db PgxConn) *TenantAssociationStore {
	return &TenantAssociationStore{db: db}
}

const tenantAssociationCheckSQL = `
SELECT EXISTS (
    SELECT 1
      FROM tenant_channel_associations
     WHERE channel = $1
       AND association = $2
       AND tenant_id = $3
)
`

// CheckAssociation implements webhook.TenantAssociationStore.
func (s *TenantAssociationStore) CheckAssociation(
	ctx context.Context,
	tenantID webhook.TenantID,
	channel, association string,
) (bool, error) {
	var ok bool
	row := s.db.QueryRow(ctx, tenantAssociationCheckSQL, channel, association, tenantID[:])
	if err := row.Scan(&ok); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// EXISTS always returns one row, but pgx may surface an
			// error if the connection is mid-flight; defensive.
			return false, nil
		}
		return false, fmt.Errorf("association check: %w", err)
	}
	return ok, nil
}
