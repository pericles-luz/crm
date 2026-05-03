// Package dnsresolver declares the hexagonal port the IAM bounded context
// uses to ask "what does the public Internet think this hostname resolves
// to?". It exists so domain code (custom-domain validation in particular)
// never reaches the network directly: callers receive a Resolver, the
// adapter at adapters/dnsresolver/miekg/ calls miekg/dns, and tests pass a
// fake.
//
// ADR 0079 §1 fixes the contract:
//
//   - LookupIP MUST return EVERY A and AAAA record observed on the wire.
//     The use-case (internal/customdomain/validation) is responsible for
//     enforcing the IP allowlist; the resolver MUST NOT silently drop
//     blocked IPs (otherwise audit observability of an SSRF attempt would
//     be lost).
//   - VerifiedWithDNSSEC MUST reflect the AD bit returned by the recursive
//     resolver. False is a perfectly valid value for hosts whose zone is
//     unsigned; ADR 0079 §2 records DNSSEC status in dns_resolution_log
//     for after-the-fact rebinding investigation but does not refuse the
//     validation.
//   - LookupTXT MUST return the concatenated character-string of each TXT
//     RR, one entry per RR. Concatenation rules follow RFC 1035 §3.3.14
//     (the resolver's job, not the caller's).
//   - The implementation MUST cap query work (deadline, retries, response
//     size) so that a hostile authoritative server cannot keep a goroutine
//     pinned. The miekg adapter sets these via context deadline + a 1 KiB
//     buffer; the in-memory test fake has no I/O so the limits are moot.
package dnsresolver

import (
	"context"
	"errors"
	"net/netip"
)

// Sentinel errors that callers MUST be able to distinguish via errors.Is.
// Any other failure (network, malformed reply, DNSSEC validation failure
// from the recursive resolver) MAY be returned wrapped or unwrapped as the
// adapter sees fit; the validation use-case treats every non-sentinel
// error as "unknown — abort with audit_log{event=customdomain_validate_unknown_error}".
var (
	// ErrNoRecord is returned when the authoritative answer is NOERROR but
	// the answer section contains no record of the requested type. This is
	// distinct from ErrNotFound (NXDOMAIN) because NOERROR/no-answer is a
	// legitimate state for hosts whose A and AAAA glue lives on a sibling
	// label.
	ErrNoRecord = errors.New("dnsresolver: no record of requested type")

	// ErrNotFound is returned for NXDOMAIN. Validation surfaces this to the
	// user as "domain does not exist" so they know to fix their DNS before
	// retrying.
	ErrNotFound = errors.New("dnsresolver: name does not exist")

	// ErrTimeout is returned when the recursive resolver did not answer in
	// time. The adapter MUST map net.OpError{timeout: true} to this so the
	// validation use-case can tell users to retry.
	ErrTimeout = errors.New("dnsresolver: deadline exceeded")
)

// IPAnswer is one A or AAAA record returned by LookupIP.
//
// IP is the literal address from the answer section, NOT a parsed/sanitised
// form. The validation use-case is responsible for SSRF allowlisting (ADR
// 0079 §1 publishes the blocked CIDRs); the adapter MUST NOT filter.
//
// VerifiedWithDNSSEC reports the AD bit on the answer that carried this IP.
// It is per-answer because in pathological mixed-zone setups a single host
// can return one signed and one unsigned RR; we surface the worst case (any
// false → false) at the use-case layer when we record dns_resolution_log.
type IPAnswer struct {
	IP                 netip.Addr
	VerifiedWithDNSSEC bool
}

// Resolver is the IAM port. The miekg adapter satisfies it with real
// queries; tests use the in-memory fake at internal/customdomain/validation/dnsfake.
type Resolver interface {
	// LookupIP MUST return ALL A and AAAA answers observed for host. An
	// empty slice with a nil error is forbidden; adapters MUST return
	// ErrNoRecord instead so callers do not have to distinguish a true
	// no-answer from a buggy adapter that swallowed records.
	LookupIP(ctx context.Context, host string) ([]IPAnswer, error)

	// LookupTXT returns each TXT RR as one string, with multi-string TXT
	// RRs already concatenated per RFC 1035. Order is the adapter's;
	// callers MUST NOT depend on it. ErrNoRecord on NOERROR/no-answer.
	LookupTXT(ctx context.Context, host string) ([]string, error)
}
