package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pericles-luz/crm/internal/slugreservation"
)

// SlugRedirectStore implements slugreservation.RedirectStore against
// the `tenant_slug_redirect` table (migration 0009).
type SlugRedirectStore struct {
	db PgxConn
}

// NewSlugRedirectStore returns a store bound to db.
func NewSlugRedirectStore(db PgxConn) *SlugRedirectStore {
	return &SlugRedirectStore{db: db}
}

const redirectActiveSQL = `
SELECT old_slug, new_slug, expires_at
  FROM tenant_slug_redirect
 WHERE old_slug = $1 AND expires_at > now()
 LIMIT 1
`

// Active returns the active redirect for old, or ErrNotReserved.
func (s *SlugRedirectStore) Active(ctx context.Context, oldSlug string) (slugreservation.Redirect, error) {
	row := s.db.QueryRow(ctx, redirectActiveSQL, oldSlug)
	var r slugreservation.Redirect
	if err := row.Scan(&r.OldSlug, &r.NewSlug, &r.ExpiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return slugreservation.Redirect{}, slugreservation.ErrNotReserved
		}
		return slugreservation.Redirect{}, fmt.Errorf("slug redirect active: %w", err)
	}
	return r, nil
}

const redirectUpsertSQL = `
INSERT INTO tenant_slug_redirect (old_slug, new_slug, expires_at, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (old_slug) DO UPDATE
   SET new_slug = EXCLUDED.new_slug,
       expires_at = EXCLUDED.expires_at,
       updated_at = now()
RETURNING old_slug, new_slug, expires_at
`

// Upsert installs or updates the redirect.
func (s *SlugRedirectStore) Upsert(ctx context.Context, oldSlug, newSlug string, expiresAt time.Time) (slugreservation.Redirect, error) {
	row := s.db.QueryRow(ctx, redirectUpsertSQL, oldSlug, newSlug, expiresAt)
	var r slugreservation.Redirect
	if err := row.Scan(&r.OldSlug, &r.NewSlug, &r.ExpiresAt); err != nil {
		return slugreservation.Redirect{}, fmt.Errorf("slug redirect upsert: %w", err)
	}
	return r, nil
}
