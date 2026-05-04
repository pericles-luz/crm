package webhook

import (
	"context"
	"errors"
	"time"
)

// TokenAdmin is the operator-facing side of webhook_tokens — used by the
// out-of-band mint CLI (cmd/webhook-token-mint) to populate and rotate
// rows. The runtime hot path uses TokenStore for lookups; this port
// exists separately so the lint scope and surface can stay narrow.
//
// Implementations MUST respect the partial unique index
// `webhook_tokens_active_idx (channel, token_hash) WHERE revoked_at IS
// NULL` declared in migration 0075a:
//
//   - Insert returns ErrTokenAlreadyActive when the row would collide
//     with an existing un-revoked row on (channel, token_hash). The
//     CLI surfaces that to the operator as an obvious copy/paste-twice
//     mistake; production should never hit it (sha256 collision).
//   - ScheduleRevocation only operates on the currently un-revoked row
//     for (channel, token_hash). It returns ErrTokenNotFound if no
//     active row exists (operator gave a wrong --rotate-from-token-hash-hex).
type TokenAdmin interface {
	// Insert appends a new active row (revoked_at IS NULL) for
	// (tenantID, channel, tokenHash) at createdAt. overlapMinutes is
	// stored as the rotation hint operators pass on the mint call so
	// dashboards can show "this token replaced X with N minutes overlap".
	Insert(ctx context.Context, tenantID TenantID, channel string, tokenHash []byte, overlapMinutes int, createdAt time.Time) error

	// ScheduleRevocation sets revoked_at = effectiveAt on the active
	// row matching (channel, tokenHash). Returns ErrTokenNotFound when
	// no active row exists. effectiveAt is computed by the caller as
	// `now + overlap_minutes * minute`; passing now itself produces an
	// immediate cut.
	ScheduleRevocation(ctx context.Context, channel string, tokenHash []byte, effectiveAt time.Time) error
}

// Admin-only sentinel errors. Distinct from the runtime ErrTokenUnknown
// / ErrTokenRevoked so the CLI can produce specific operator messages.
var (
	// ErrTokenAlreadyActive is returned by TokenAdmin.Insert when an
	// active row already exists for (channel, token_hash). In practice
	// the operator pasted the same token twice or the partial unique
	// index has drifted.
	ErrTokenAlreadyActive = errors.New("webhook: token already active for (channel, token_hash)")

	// ErrTokenNotFound is returned by TokenAdmin.ScheduleRevocation
	// when no active row matches the (channel, token_hash) pair the
	// operator supplied.
	ErrTokenNotFound = errors.New("webhook: no active token for (channel, token_hash)")
)
