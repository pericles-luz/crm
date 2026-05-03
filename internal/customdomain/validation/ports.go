package validation

import (
	"context"
	"time"
)

// Auditor records security-relevant outcomes. The validation use-case fires
// exactly one audit event per Validate call: the success event on the
// happy path, or an error-specific event on each failure branch (including
// the SSRF block, which is the most important one — F43 turned on
// exactly because we did not have observability of attempted private-IP
// targeting before).
//
// The contract is fire-and-forget for the caller: a failing Auditor MUST
// NOT fail the validation. We log the audit failure as best we can and
// continue, because losing a single audit row is worse than losing the
// validation itself (which the user can retry; the audit row cannot be
// reconstructed). This mirrors how internal/wallet/usecase handles its
// metrics port.
type Auditor interface {
	Record(ctx context.Context, event AuditEvent)
}

// AuditEvent is the structured payload Auditor receives. The Event field
// is the controlled vocabulary; PII-safe Detail keys are the only escape
// hatch. Resolved IPs are intentionally omitted from blocked-SSRF events
// to avoid mirroring the attacker's chosen address back through our log
// pipeline; only the host (which the attacker already knew) is recorded.
type AuditEvent struct {
	// Event is one of the constants in this file. Anything else MUST
	// either be added here or rejected at compile time (callers cannot
	// construct other values without importing the constant).
	Event string
	// Host is the hostname the user asked us to validate. Always present.
	Host string
	// Detail carries optional PII-safe key/values (e.g. "dnssec=true").
	// Adapters MAY persist this as JSON; tests inspect it directly.
	Detail map[string]string
	// At is the wall-clock timestamp the event was generated. The
	// validator fills this from the Clock port so tests can pin time.
	At time.Time
}

// The audit event vocabulary. Adding an entry is intentionally awkward
// (must be exported, must appear in this file, must be referenced from
// validate.go) so reviewers see every new pathway.
const (
	EventValidatedOK         = "customdomain_validate_ok"
	EventBlockedSSRF         = "customdomain_validate_blocked_ssrf"
	EventTokenMismatch       = "customdomain_validate_token_mismatch"
	EventNoAddress           = "customdomain_validate_no_address"
	EventResolverError       = "customdomain_validate_resolver_error"
	EventEmptyInput          = "customdomain_validate_empty_input"
)

// Clock is a 1-method port over time.Now so tests can pin VerifiedAt and
// AuditEvent.At deterministically. It mirrors port.Clock from the wallet
// package; we re-declare it instead of importing because validation must
// stay free of any other bounded context.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production Clock — never used in tests.
type SystemClock struct{}

// Now returns time.Now in UTC so persisted timestamps are unambiguous.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// noopAuditor is the safe fallback when callers pass a nil Auditor. It is
// not exported because callers SHOULD pass an explicit Auditor; the safety
// net is here so a wiring bug does not strand a panic in the validation
// hot path.
type noopAuditor struct{}

func (noopAuditor) Record(context.Context, AuditEvent) {}

