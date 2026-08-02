package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	channelinstagram "github.com/pericles-luz/crm/internal/adapter/channel/instagram"
)

// InstagramOAuthTokenStore persists the per-tenant Instagram Business
// Login access token (migration 0138_instagram_oauth_tokens), keyed by
// tenant_id alone — the outbound Instagram wiring already assumes
// exactly one Instagram channel per tenant (see
// instagramOutboundIGBusinessID in cmd/server/instagram_outbound_wire.go).
// Mirrors ChannelAssociationWriter's shape: no RLS on this table (it's
// composition-root/admin-flow config, not tenant runtime data queried
// under app_runtime's row-level policies).
type InstagramOAuthTokenStore struct {
	db PgxConn
}

// NewInstagramOAuthTokenStore returns a store bound to db.
func NewInstagramOAuthTokenStore(db PgxConn) *InstagramOAuthTokenStore {
	return &InstagramOAuthTokenStore{db: db}
}

// instagramOAuthTokenUpsertSQL upserts on the tenant_id PRIMARY KEY so a
// reconnect (fresh Business Login run) replaces the previous token
// instead of failing on a duplicate key.
const instagramOAuthTokenUpsertSQL = `
INSERT INTO instagram_oauth_tokens (tenant_id, access_token, token_type, expires_at, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (tenant_id) DO UPDATE SET
    access_token = EXCLUDED.access_token,
    token_type   = EXCLUDED.token_type,
    expires_at   = EXCLUDED.expires_at,
    updated_at   = now()
`

// Save upserts the tenant's long-lived access token. Implements
// channelinstagram.OAuthTokenSaver.
func (s *InstagramOAuthTokenStore) Save(ctx context.Context, tenantID uuid.UUID, accessToken, tokenType string, expiresAt time.Time) error {
	if _, err := s.db.Exec(ctx, instagramOAuthTokenUpsertSQL, tenantID, accessToken, tokenType, expiresAt); err != nil {
		return fmt.Errorf("save instagram oauth token: %w", err)
	}
	return nil
}

const instagramOAuthTokenSelectSQL = `
SELECT access_token, expires_at
  FROM instagram_oauth_tokens
 WHERE tenant_id = $1
`

// Get returns the tenant's stored access token. ok=false (no error)
// means the tenant hasn't connected Instagram via Business Login yet —
// callers fall back to the legacy global token, if any.
func (s *InstagramOAuthTokenStore) Get(ctx context.Context, tenantID uuid.UUID) (accessToken string, expiresAt time.Time, ok bool, err error) {
	err = s.db.QueryRow(ctx, instagramOAuthTokenSelectSQL, tenantID).Scan(&accessToken, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("get instagram oauth token: %w", err)
	}
	return accessToken, expiresAt, true, nil
}

// Compile-time assertion that Save satisfies the OAuth callback
// handler's persistence port.
var _ channelinstagram.OAuthTokenSaver = (*InstagramOAuthTokenStore)(nil)
