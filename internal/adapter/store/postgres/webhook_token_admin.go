package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pericles-luz/crm/internal/webhook"
)

// TokenAdmin implements webhook.TokenAdmin against Postgres for the
// out-of-band mint CLI. The runtime hot path uses TokenStore in the
// same package; the surfaces are split because the lint / review
// burden for the admin path is lower (it only runs from a CLI, never
// inside the webhook request handler).
type TokenAdmin struct {
	db PgxConn
}

// NewTokenAdmin returns a TokenAdmin bound to the given pgx pool/conn.
func NewTokenAdmin(db PgxConn) *TokenAdmin { return &TokenAdmin{db: db} }

// uniqueViolationCode is the SQLSTATE Postgres returns when an INSERT
// trips a unique index. We map that to webhook.ErrTokenAlreadyActive
// rather than a generic database error so the CLI can give the
// operator a useful message.
const uniqueViolationCode = "23505"

const tokenAdminInsertSQL = `
INSERT INTO webhook_tokens (tenant_id, channel, token_hash, overlap_minutes, created_at)
VALUES ($1, $2, $3, $4, $5)
`

// Insert appends a new active row for (tenantID, channel, tokenHash).
// Returns webhook.ErrTokenAlreadyActive when the partial unique index
// rejects the insert (an un-revoked row already exists for the pair).
func (a *TokenAdmin) Insert(
	ctx context.Context,
	tenantID webhook.TenantID,
	channel string,
	tokenHash []byte,
	overlapMinutes int,
	createdAt time.Time,
) error {
	_, err := a.db.Exec(ctx, tokenAdminInsertSQL, tenantID[:], channel, tokenHash, overlapMinutes, createdAt)
	if err == nil {
		return nil
	}
	if isUniqueViolation(err) {
		return webhook.ErrTokenAlreadyActive
	}
	return fmt.Errorf("webhook_tokens insert: %w", err)
}

const tokenAdminScheduleRevokeSQL = `
UPDATE webhook_tokens
   SET revoked_at = $3
 WHERE channel = $1 AND token_hash = $2 AND revoked_at IS NULL
`

// ScheduleRevocation sets revoked_at on the active row matching
// (channel, tokenHash). Returns webhook.ErrTokenNotFound when no row
// is updated — typically the operator gave a wrong hash.
func (a *TokenAdmin) ScheduleRevocation(
	ctx context.Context,
	channel string,
	tokenHash []byte,
	effectiveAt time.Time,
) error {
	tag, err := a.db.Exec(ctx, tokenAdminScheduleRevokeSQL, channel, tokenHash, effectiveAt)
	if err != nil {
		return fmt.Errorf("webhook_tokens schedule revoke: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return webhook.ErrTokenNotFound
	}
	return nil
}

// isUniqueViolation reports whether err is a Postgres unique-violation,
// matched by SQLSTATE 23505. The pgx error chain may carry either a
// *pgconn.PgError or a wrapper; we accept both.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == uniqueViolationCode
	}
	// Defence in depth: some test fakes only return a string-formed error,
	// so allow callers to surface the SQLSTATE in the message.
	return strings.Contains(err.Error(), uniqueViolationCode)
}

// Compile-time assertion that *TokenAdmin satisfies the port.
var _ webhook.TokenAdmin = (*TokenAdmin)(nil)
