package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/pericles-luz/crm/internal/customdomain/enrollment"
)

// EnrollmentCountStore is the production implementation of
// enrollment.CountStore (SIN-62334 F53). It counts active (non-deleted)
// rows in tenant_custom_domains for the given tenant — the same source
// of truth the management UI lists from. The 25-active hard cap is
// checked against this count before the rolling-window quotas hit
// Redis.
//
// Why Postgres and not Redis (despite the AC literal text): the source
// of truth for "active (non-deleted) domain count" IS the Postgres
// table. Backing this with a Redis SET would require maintaining
// SADD/SREM membership in lock-step with every soft-delete, with no
// transactional guarantee — a Redis loss would silently bypass the
// hard cap. One indexed COUNT(*) against the same partial-unique index
// the UI already hits is correct-by-design and zero new failure modes.
//
// One indexed SELECT against the partial unique index
// uq_tenant_custom_domains_active_host (declared in
// 0010_tenant_custom_domains.up.sql) — the WHERE deleted_at IS NULL clause
// matches the same predicate the index uses.
type EnrollmentCountStore struct {
	db PgxConn
}

// NewEnrollmentCountStore binds the count adapter to db.
func NewEnrollmentCountStore(db PgxConn) *EnrollmentCountStore {
	return &EnrollmentCountStore{db: db}
}

const enrollmentActiveCountSQL = `
SELECT COUNT(*)::bigint
  FROM tenant_custom_domains
 WHERE tenant_id = $1
   AND deleted_at IS NULL
`

// ActiveCount implements enrollment.CountStore.
func (s *EnrollmentCountStore) ActiveCount(ctx context.Context, tenantID uuid.UUID) (int, error) {
	if tenantID == uuid.Nil {
		return 0, errors.New("enrollment_count: tenantID must not be uuid.Nil")
	}
	row := s.db.QueryRow(ctx, enrollmentActiveCountSQL, tenantID)
	var n int64
	if err := row.Scan(&n); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("enrollment_count: %w", err)
	}
	if n < 0 {
		return 0, nil
	}
	return int(n), nil
}

var _ enrollment.CountStore = (*EnrollmentCountStore)(nil)
