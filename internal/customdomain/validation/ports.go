package validation

import (
	"context"
	"net/netip"
	"time"

	"github.com/google/uuid"
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
	EventValidatedOK   = "customdomain_validate_ok"
	EventBlockedSSRF   = "customdomain_validate_blocked_ssrf"
	EventTokenMismatch = "customdomain_validate_token_mismatch"
	EventNoAddress     = "customdomain_validate_no_address"
	EventResolverError = "customdomain_validate_resolver_error"
	EventEmptyInput    = "customdomain_validate_empty_input"
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

// Writer persists one row per Validate / ValidateHostOnly call to the
// dns_resolution_log table (ADR 0079 §1 step 4 / OWASP A09). It is
// distinct from Auditor because adapters differ: the Auditor is a
// fire-and-forget log sink, while the Writer is a transactional store
// that must keep tenant_id, decision, and reason in a queryable column
// for IR forensics.
//
// The Validator calls Write exactly once per terminal path (success,
// block, or error). On Writer failure the validator MUST NOT fail the
// caller — losing one audit row is preferable to denying a legitimate
// validation. Adapters log internal errors and return nil on the hot
// path; we still let Write return an error so test doubles can assert
// the call shape without swallowing scan bugs.
type Writer interface {
	Write(ctx context.Context, entry LogEntry) error
}

// LogEntry is the canonical row shape persisted to dns_resolution_log.
// Field semantics:
//
//   - TenantID: the tenant initiating the validation. Zero (uuid.Nil)
//     when the call originates outside a tenant request — for example
//     a startup self-test. Adapters MUST persist NULL on uuid.Nil so
//     forensics can distinguish anonymous attempts from a known tenant.
//   - Host: the hostname under validation, normalized to lowercase.
//   - PinnedIP: the resolved IP we pinned on success. Zero (Addr{}) on
//     blocked-SSRF and on every error path. Adapters MUST persist NULL
//     on a zero Addr — the blocked-SSRF case INTENTIONALLY discards the
//     attacker-chosen IP so it never lands in our log pipeline.
//   - VerifiedWithDNSSEC: only meaningful when PinnedIP.IsValid(). On
//     blocked / error rows it is false by convention.
//   - Decision: controlled vocabulary; one of DecisionAllow,
//     DecisionBlock, or DecisionError. Adapters store it verbatim.
//   - Reason: short controlled-vocabulary string explaining the decision
//     (ReasonOK, ReasonPrivateIP, ReasonTokenMismatch, etc.). Mirrors the
//     Auditor event vocabulary so the two stores can be cross-checked.
//   - Phase: PhaseValidate or PhaseHostOnly — distinguishes Verify
//     (full check) from the pre-flight at Enroll time.
//   - At: the wall-clock timestamp the validator generated the row,
//     pulled from the same Clock the auditor sees.
type LogEntry struct {
	TenantID           uuid.UUID
	Host               string
	PinnedIP           netip.Addr
	VerifiedWithDNSSEC bool
	Decision           string
	Reason             string
	Phase              string
	At                 time.Time
}

// Decision values stored in dns_resolution_log.decision.
const (
	DecisionAllow = "allow"
	DecisionBlock = "block"
	DecisionError = "error"
)

// Reason vocabulary stored in dns_resolution_log.reason. Mirrors the
// Auditor event vocabulary so an IR engineer can join logs by reason
// without translating between two strings tables.
const (
	ReasonOK             = "ok"
	ReasonPrivateIP      = "private_ip"
	ReasonTokenMismatch  = "token_mismatch"
	ReasonNoAddress      = "no_address"
	ReasonResolverError  = "resolver_error"
	ReasonEmptyInput     = "empty_input"
)

// Phase values stored in dns_resolution_log.phase.
const (
	PhaseValidate = "validate"
	PhaseHostOnly = "host_only"
)

// noopWriter is the safe fallback when callers do not configure a Writer.
// Same rationale as noopAuditor: a missing Writer must not strand the hot
// path on a nil-pointer panic.
type noopWriter struct{}

func (noopWriter) Write(context.Context, LogEntry) error { return nil }
