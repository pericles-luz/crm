package validation

import (
	"errors"
	"net/netip"
	"time"
)

// Errors returned by Validate. Callers MUST distinguish them with errors.Is
// so they can map to the right HTTP response and audit_log{event=…} value.
var (
	// ErrPrivateIP is returned when at least one A/AAAA answer for the host
	// resolves to a CIDR listed in blocklist.go. The corresponding audit
	// event is "customdomain_validate_blocked_ssrf"; the response carries
	// no IP back to the user (defence in depth — do not leak the resolved
	// address to the attacker who controls the zone).
	ErrPrivateIP = errors.New("validation: host resolves to a blocked private/loopback range")

	// ErrTokenMismatch is returned when no TXT record under
	// _crm-verify.<host> contained the expected ownership token. The audit
	// event is "customdomain_validate_token_mismatch".
	ErrTokenMismatch = errors.New("validation: ownership token not found in TXT record")

	// ErrNoAddress is returned when the host has neither A nor AAAA. We
	// surface this so the user gets actionable feedback ("publish A/AAAA
	// before retrying") instead of a generic resolver error.
	ErrNoAddress = errors.New("validation: host has no A or AAAA record")

	// ErrEmptyHost is returned when the caller passes "". This is a
	// programmer error, not a user-visible state, but we keep it as a
	// sentinel so the upstream handler can return 500 (caller bug) rather
	// than 400 (user input problem).
	ErrEmptyHost = errors.New("validation: host is empty")

	// ErrEmptyToken is the same shape as ErrEmptyHost but for the
	// expectedToken argument. Custom-domain rows without a verification
	// token MUST never reach Validate (issue B forbids it at the handler);
	// returning a sentinel here makes that contract testable.
	ErrEmptyToken = errors.New("validation: expected token is empty")
)

// Result is the audit-grade record of a successful Validate call. It is
// populated only when err == nil; on the error path callers MUST NOT use
// these fields (in particular, IP is the zero value, NOT the resolved IP
// of a blocked SSRF target — that would be a leak).
//
// IP is the single IP that callers MUST pin in dns_resolution_log and use
// for any subsequent "is this still the same host" checks (ADR 0079 §2,
// anti-rebinding). It is the first non-blocked answer seen; the order is
// the resolver's order, which is good enough because callers re-pin on
// every renewal.
type Result struct {
	IP                 netip.Addr
	VerifiedAt         time.Time
	VerifiedWithDNSSEC bool
}
