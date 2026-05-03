package validation

import (
	"context"
	"fmt"
	"strings"

	"github.com/pericles-luz/crm/internal/iam/dnsresolver"
)

// txtSubdomain is the label we publish in customer-facing instructions:
// "create a TXT record at _crm-verify.<your-host> containing <token>". It
// is a constant rather than a configurable so an instance cannot be set
// up to look up TXTs at the apex (which would let any operator who can
// publish a TXT for some other reason bypass ownership proof).
const txtSubdomain = "_crm-verify."

// Validator is the use-case for ADR 0079 §1. It is constructed once at
// boot with concrete adapters, then handed to whatever HTTP/handler layer
// needs to verify ownership.
//
// The struct holds dependencies, never per-request state. It is safe for
// concurrent use as long as the adapters it wraps are.
type Validator struct {
	resolver dnsresolver.Resolver
	auditor  Auditor
	clock    Clock
}

// New builds a Validator. Passing a nil Auditor is allowed and uses a
// silent fallback (see noopAuditor in ports.go); passing a nil Clock or
// nil Resolver is a programmer error and will panic on the first call,
// because the alternative — silently using time.Now and a no-op resolver —
// would silently weaken the security contract.
//
// We do not panic at construction time so this can be called from main
// before adapters are wired (e.g. in graceful-shutdown ordering tests).
func New(resolver dnsresolver.Resolver, auditor Auditor, clock Clock) *Validator {
	if auditor == nil {
		auditor = noopAuditor{}
	}
	return &Validator{resolver: resolver, auditor: auditor, clock: clock}
}

// Validate is the one entry point. The contract is:
//
//   - Empty host or expectedToken → ErrEmptyHost / ErrEmptyToken (programmer
//     error; audit fired with EventEmptyInput so a misconfigured caller is
//     visible in the audit log).
//   - Resolver error on either lookup → wrapped error with %w semantics;
//     EventResolverError. Caller MAY retry.
//   - Host has no A/AAAA → ErrNoAddress + EventNoAddress.
//   - ANY answer falls in a blocked CIDR → ErrPrivateIP + EventBlockedSSRF.
//     We REJECT even when only one of N answers is blocked, because a
//     mixed answer is a textbook DNS-rebinding setup.
//   - No TXT under _crm-verify.<host> matches expectedToken → ErrTokenMismatch
//     + EventTokenMismatch.
//   - Success → Result populated + EventValidatedOK.
//
// On success, Result.IP is the FIRST non-blocked IP returned by the
// resolver; callers MUST persist this in dns_resolution_log so a follow-up
// rebind from the same host is observable (ADR 0079 §2).
func (v *Validator) Validate(ctx context.Context, host, expectedToken string) (Result, error) {
	host = strings.TrimSpace(strings.ToLower(host))
	expectedToken = strings.TrimSpace(expectedToken)
	now := v.clock.Now()

	if host == "" {
		v.auditor.Record(ctx, AuditEvent{Event: EventEmptyInput, Host: host, At: now, Detail: map[string]string{"reason": "host"}})
		return Result{}, ErrEmptyHost
	}
	if expectedToken == "" {
		v.auditor.Record(ctx, AuditEvent{Event: EventEmptyInput, Host: host, At: now, Detail: map[string]string{"reason": "token"}})
		return Result{}, ErrEmptyToken
	}

	answers, err := v.resolver.LookupIP(ctx, host)
	if err != nil {
		v.auditor.Record(ctx, AuditEvent{Event: EventResolverError, Host: host, At: now, Detail: map[string]string{"phase": "ip", "err": err.Error()}})
		return Result{}, fmt.Errorf("validation: lookup IP for %q: %w", host, err)
	}
	if len(answers) == 0 {
		v.auditor.Record(ctx, AuditEvent{Event: EventNoAddress, Host: host, At: now})
		return Result{}, ErrNoAddress
	}

	// SSRF guard: reject if ANY answer is blocked. We intentionally do not
	// log the blocked IP itself (see Auditor doc): the host is enough for
	// the alert, and including the IP would mirror the attacker's chosen
	// address through our log pipeline.
	dnssecForPick := true
	var pick dnsresolver.IPAnswer
	picked := false
	for _, a := range answers {
		if isBlocked(a.IP) {
			v.auditor.Record(ctx, AuditEvent{Event: EventBlockedSSRF, Host: host, At: now, Detail: map[string]string{"answers": fmt.Sprintf("%d", len(answers))}})
			return Result{}, ErrPrivateIP
		}
		// AND-style: if any answer is unsigned, the verified-with-DNSSEC
		// flag we persist drops to false. That gives anti-rebinding
		// reviewers an unambiguous signal.
		if !a.VerifiedWithDNSSEC {
			dnssecForPick = false
		}
		if !picked {
			pick = a
			picked = true
		}
	}

	// TXT proof of ownership.
	txts, err := v.resolver.LookupTXT(ctx, txtSubdomain+host)
	if err != nil {
		v.auditor.Record(ctx, AuditEvent{Event: EventResolverError, Host: host, At: now, Detail: map[string]string{"phase": "txt", "err": err.Error()}})
		return Result{}, fmt.Errorf("validation: lookup TXT for %q: %w", txtSubdomain+host, err)
	}
	if !containsToken(txts, expectedToken) {
		v.auditor.Record(ctx, AuditEvent{Event: EventTokenMismatch, Host: host, At: now, Detail: map[string]string{"records": fmt.Sprintf("%d", len(txts))}})
		return Result{}, ErrTokenMismatch
	}

	res := Result{
		IP:                 pick.IP,
		VerifiedAt:         now,
		VerifiedWithDNSSEC: dnssecForPick,
	}
	v.auditor.Record(ctx, AuditEvent{
		Event: EventValidatedOK,
		Host:  host,
		At:    now,
		Detail: map[string]string{
			"ip":     res.IP.String(),
			"dnssec": fmt.Sprintf("%t", res.VerifiedWithDNSSEC),
		},
	})
	return res, nil
}

// containsToken does a constant-style equality check in a loop. The token
// is a server-issued opaque string — there is no timing-side-channel risk
// because the attacker controls the TXT side, not our side, and they get
// no signal beyond pass/fail. Using == here keeps the implementation
// boring and easy to review.
func containsToken(txts []string, expected string) bool {
	for _, t := range txts {
		if strings.TrimSpace(t) == expected {
			return true
		}
	}
	return false
}
