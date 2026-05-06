package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pericles-luz/crm/internal/slugreservation"
)

// SlugReservationStore implements slugreservation.Store against the
// `tenant_slug_reservation` table (migration 0008). It is stateless;
// the caller supplies the *pgxpool.Pool wrapped in WithTenant /
// WithMasterOps as the operation requires.
type SlugReservationStore struct {
	db PgxConn
}

// NewSlugReservationStore returns a SlugReservationStore bound to db.
func NewSlugReservationStore(db PgxConn) *SlugReservationStore {
	return &SlugReservationStore{db: db}
}

// activeSelectSQL reads the currently-active reservation for the slug.
// "Active" = master has not released it AND it has not naturally expired.
// The partial unique index (tenant_slug_reservation_active_idx) covers
// the master-released predicate; the planner adds the expires_at filter.
const activeSelectSQL = `
SELECT id, slug, released_at, released_by_tenant_id, expires_at, created_at
  FROM tenant_slug_reservation
 WHERE slug = $1
   AND released_by_master IS FALSE
   AND expires_at > now()
 LIMIT 1
`

// Active returns the active reservation for slug, or
// slugreservation.ErrNotReserved if no row qualifies.
func (s *SlugReservationStore) Active(ctx context.Context, slug string) (slugreservation.Reservation, error) {
	row := s.db.QueryRow(ctx, activeSelectSQL, slug)
	return scanReservation(row)
}

// reservationInsertSQL appends a row. The partial unique index turns
// "another active reservation already exists" into a unique-violation
// (SQLSTATE 23505), which we surface as a *ReservedError.
const reservationInsertSQL = `
INSERT INTO tenant_slug_reservation (slug, released_at, released_by_tenant_id, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING id, slug, released_at, released_by_tenant_id, expires_at, created_at
`

// Insert performs the release-time INSERT.
func (s *SlugReservationStore) Insert(ctx context.Context, slug string, byTenant uuid.UUID, releasedAt, expiresAt time.Time) (slugreservation.Reservation, error) {
	var byArg any
	if byTenant != uuid.Nil {
		byArg = byTenant
	}
	row := s.db.QueryRow(ctx, reservationInsertSQL, slug, releasedAt, byArg, expiresAt)
	res, err := scanReservation(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Active reservation already exists — re-read the row so
			// callers receive the existing reservation in the wrapped
			// error.
			if existing, readErr := s.Active(ctx, slug); readErr == nil {
				return slugreservation.Reservation{}, &slugreservation.ReservedError{Reservation: existing}
			}
		}
		return slugreservation.Reservation{}, err
	}
	return res, nil
}

// reservationSoftDeleteSQL flips released_by_master and stamps
// expires_at = $2 (now()) on the active row. RETURNING fires when a row
// matched; if no rows update the call returns ErrNotReserved.
const reservationSoftDeleteSQL = `
UPDATE tenant_slug_reservation
   SET expires_at = $2,
       released_by_master = TRUE
 WHERE slug = $1
   AND released_by_master IS FALSE
   AND expires_at > $2
RETURNING id, slug, released_at, released_by_tenant_id, expires_at, created_at
`

// SoftDelete is the master-override soft-delete.
func (s *SlugReservationStore) SoftDelete(ctx context.Context, slug string, at time.Time) (slugreservation.Reservation, error) {
	row := s.db.QueryRow(ctx, reservationSoftDeleteSQL, slug, at)
	res, err := scanReservation(row)
	if errors.Is(err, slugreservation.ErrNotReserved) {
		return slugreservation.Reservation{}, slugreservation.ErrNotReserved
	}
	return res, err
}

// scanReservation reads the standard SELECT/RETURNING shape into a
// Reservation. ErrNoRows is mapped to slugreservation.ErrNotReserved so
// callers can errors.Is without importing pgx.
func scanReservation(row pgx.Row) (slugreservation.Reservation, error) {
	var (
		id                               [16]byte
		slugStr                          string
		releasedAt, expiresAt, createdAt time.Time
		releasedBy                       *[16]byte
	)
	if err := row.Scan(&id, &slugStr, &releasedAt, &releasedBy, &expiresAt, &createdAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return slugreservation.Reservation{}, slugreservation.ErrNotReserved
		}
		return slugreservation.Reservation{}, fmt.Errorf("slug reservation scan: %w", err)
	}
	r := slugreservation.Reservation{
		ID:         uuid.UUID(id),
		Slug:       slugStr,
		ReleasedAt: releasedAt,
		ExpiresAt:  expiresAt,
		CreatedAt:  createdAt,
	}
	if releasedBy != nil {
		r.ReleasedByTenantID = uuid.UUID(*releasedBy)
	}
	return r, nil
}
