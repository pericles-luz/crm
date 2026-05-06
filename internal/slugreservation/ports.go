package slugreservation

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Store is the persistence port for slug reservations. The Postgres
// adapter in internal/adapter/store/postgres satisfies it.
//
// Active is the hot read used by RequireSlugAvailable: returns the row
// if any reservation has expires_at > now() for the slug, ErrNotReserved
// otherwise.
//
// Insert performs the release-time INSERT and is allowed to runtime;
// the partial unique index on the table prevents accidental duplicate
// active reservations.
//
// SoftDelete is the master override path: it stamps expires_at = now()
// on the active reservation. Returns ErrNotReserved if there is no
// active row to override. The caller is expected to invoke this inside
// WithMasterOps so the master_ops_audit_trigger fires.
type Store interface {
	Active(ctx context.Context, slug string) (Reservation, error)
	Insert(ctx context.Context, slug string, releasedByTenantID uuid.UUID, releasedAt time.Time, expiresAt time.Time) (Reservation, error)
	SoftDelete(ctx context.Context, slug string, at time.Time) (Reservation, error)
}

// RedirectStore is the persistence port for slug redirects.
//
// Active returns the redirect with expires_at > now() for old, or
// ErrNotReserved-style miss (handlers translate that to 404).
//
// Upsert installs/updates a redirect, used by the slug-change flow.
// Always called from app_master_ops because slug changes are
// privileged.
type RedirectStore interface {
	Active(ctx context.Context, oldSlug string) (Redirect, error)
	Upsert(ctx context.Context, oldSlug, newSlug string, expiresAt time.Time) (Redirect, error)
}

// Clock is the time port. Tests inject a fixed clock so window math is
// reproducible. The default is time.Now via SystemClock.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production Clock implementation; var so tests can
// substitute it where a struct field is awkward.
type SystemClock struct{}

// Now returns time.Now in UTC.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// MasterAuditLogger is the structured-event sink for master overrides.
// The default adapter writes a slog record at info level with a
// machine-grep-able event tag; the master_ops_audit table also captures
// the underlying UPDATE via the trigger, but that is row-level and does
// not capture the human reason — this port is what carries the reason
// string into long-term storage.
type MasterAuditLogger interface {
	LogMasterOverride(ctx context.Context, ev MasterOverrideEvent) error
}

// SlackNotifier is the immediate-alert sink. The Slack channel is
// configured at adapter construction; this surface only knows "alert".
// Errors are intentionally non-fatal at the use-case level (logged,
// not propagated to the master) so a downed Slack does not block an
// override the master already authorized.
type SlackNotifier interface {
	NotifyAlert(ctx context.Context, msg string) error
}

// MasterAuthorizer extracts the authenticated master identity from the
// request and reports whether MFA was satisfied. The MFA bit is
// consulted by the override handler so we can:
//
//   - require MFA (return 403) once SIN-62223 (F15) ships RequireMasterMFA;
//   - or accept a master without MFA today, gated by a config flag, so
//     F46 can land before F15. ADR 0079 §4 documents the flag.
//
// Implementations live alongside the rest of the auth glue; this port
// keeps the use-case package free of net/http auth wiring.
type MasterAuthorizer interface {
	AuthorizeMaster(ctx context.Context) (masterID uuid.UUID, mfaPresent bool, err error)
}
