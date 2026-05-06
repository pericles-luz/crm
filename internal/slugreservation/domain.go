package slugreservation

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ReservationWindow is the 12-month lock applied when a slug is
// released. Hard-coded per the F46 spec; if it ever needs tuning it
// should grow into config rather than a magic number scattered around.
const ReservationWindow = 365 * 24 * time.Hour

// RedirectWindow is the same 12-month horizon applied to old → new slug
// redirects. They share the value but are conceptually independent — a
// future ADR could split them without touching call sites.
const RedirectWindow = ReservationWindow

// ClearSiteDataCookies is the value emitted on the redirect response so
// that any leftover session cookie on the old subdomain is wiped before
// the browser lands on the new one.
const ClearSiteDataCookies = `"cookies"`

// MaxSlugLen caps slug length defensively at the boundary. Postgres
// stores text without limit but path/host components stay sane below
// 64 bytes.
const MaxSlugLen = 63

// slugPattern accepts the conventional "lowercase letters, digits, and
// internal hyphens" subdomain shape. Anchored, no leading/trailing
// hyphen, at least one char.
var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

// Reservation describes an active slug reservation row. Returned by
// Store.Active to feed both the middleware 409 response and the master
// console.
type Reservation struct {
	ID                  uuid.UUID
	Slug                string
	ReleasedAt          time.Time
	ReleasedByTenantID  uuid.UUID // uuid.Nil when forensic pointer unknown.
	ExpiresAt           time.Time
	CreatedAt           time.Time
}

// Redirect describes an active slug redirect row.
type Redirect struct {
	OldSlug   string
	NewSlug   string
	ExpiresAt time.Time
}

// MasterOverrideEvent carries the per-override forensic data that the
// audit logger and Slack notifier need. It travels by value because
// it is small and immutable.
type MasterOverrideEvent struct {
	Slug     string
	MasterID uuid.UUID
	Reason   string
	At       time.Time
}

// Sentinel errors. Use errors.Is to test.
var (
	// ErrInvalidSlug is returned for any slug that fails NormalizeSlug.
	ErrInvalidSlug = errors.New("slugreservation: invalid slug")

	// ErrSlugReserved is returned when a request hits an active
	// reservation. Wrap with %w to preserve the Reservation payload.
	ErrSlugReserved = errors.New("slugreservation: slug reserved")

	// ErrNotReserved is returned by OverrideRelease when the override
	// path is asked to free a slug that has no active reservation.
	ErrNotReserved = errors.New("slugreservation: no active reservation")

	// ErrReasonRequired is returned when the master override is called
	// without a non-empty reason — least-privilege rule, every override
	// must justify itself in writing for the audit trail.
	ErrReasonRequired = errors.New("slugreservation: override reason required")

	// ErrZeroMaster is returned when OverrideRelease receives uuid.Nil.
	ErrZeroMaster = errors.New("slugreservation: master id required")

	// ErrUnauthorized is returned by HTTP handlers when the request is
	// not authenticated as master. 403, no body.
	ErrUnauthorized = errors.New("slugreservation: unauthorized")
)

// ReservedError carries the reservation payload alongside ErrSlugReserved
// so handlers can produce a structured 409 body without re-fetching.
// Use errors.As to extract.
type ReservedError struct {
	Reservation Reservation
}

func (e *ReservedError) Error() string {
	return "slugreservation: slug \"" + e.Reservation.Slug +
		"\" reserved until " + e.Reservation.ExpiresAt.UTC().Format("2006-01-02")
}

// Unwrap exposes ErrSlugReserved so errors.Is(err, ErrSlugReserved)
// works regardless of whether the caller has the payload or just the
// sentinel.
func (e *ReservedError) Unwrap() error { return ErrSlugReserved }

// NormalizeSlug lowercases, trims, and validates a slug. It returns
// ErrInvalidSlug for empty strings, anything past MaxSlugLen, or
// anything that does not match slugPattern.
func NormalizeSlug(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > MaxSlugLen {
		return "", ErrInvalidSlug
	}
	if !slugPattern.MatchString(s) {
		return "", ErrInvalidSlug
	}
	return s, nil
}

// FormatExpiresAt renders the date used in the 409 message. UTC and
// YYYY-MM-DD per spec.
func FormatExpiresAt(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}
