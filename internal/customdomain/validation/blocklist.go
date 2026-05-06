// Package validation is the pure-domain custom-domain ownership validator
// for ADR 0079 §1 (SIN-62242). It is reachable from the on-demand TLS guard
// (issue B, SIN-62243) and from the tenant-facing setup endpoint (issue
// SIN-62259) — no other path accepts an attacker-controlled hostname.
//
// Purity contract:
//
//   - This package does NOT import net/http (enforced at compile time by the
//     internal/lint/customdomainnet analyzer; CI fails on violation).
//   - It does NOT issue I/O directly. Every external call goes through a
//     hexagonal port: dnsresolver.Resolver, Auditor, Clock.
//   - It does not return or log raw DNS reply bodies; only the structured
//     Result + audit_log{event=…} record reach storage.
package validation

import (
	"net/netip"
)

// blockedNets is the SSRF allowlist (negative form) fixed by ADR 0079 §1.
// It is private to this package because there is exactly one correct list
// — the security review baseline; any caller that wants a different list is
// trying to bypass the gate.
//
// The list covers, in order: RFC 1918 private space, IANA loopback, RFC
// 3927 link-local (the IMDS range 169.254.169.254 lives here), CGNAT
// (RFC 6598), the "this network" placeholder (RFC 1122), the multicast
// range, IPv6 loopback, IPv6 ULA (RFC 4193), and IPv6 link-local. The
// list mirrors infra/caddy/unbound.conf so a host blocked at the
// sidecar is also blocked here, and vice versa.
var blockedPrefixes = func() []netip.Prefix {
	raw := []string{
		"10.0.0.0/8",     // RFC 1918
		"172.16.0.0/12",  // RFC 1918
		"192.168.0.0/16", // RFC 1918
		"127.0.0.0/8",    // loopback
		"169.254.0.0/16", // link-local incl. AWS/GCP IMDS 169.254.169.254
		"100.64.0.0/10",  // RFC 6598 CGNAT
		"0.0.0.0/8",      // "this network", RFC 1122
		"224.0.0.0/4",    // multicast
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 unique-local (RFC 4193)
		"fe80::/10",      // IPv6 link-local
	}
	out := make([]netip.Prefix, 0, len(raw))
	for _, s := range raw {
		// netip.MustParsePrefix is safe here because the input is a fixed
		// literal list reviewed in the ADR; a panic at init is the right
		// failure mode if a future edit makes it malformed.
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// isBlocked reports whether ip falls inside any range listed in the ADR.
// The check uses netip.Prefix.Contains, which already normalises v4-mapped
// v6 (::ffff:127.0.0.1) into the v4 form on the addr side, so an attacker
// cannot smuggle 127.0.0.1 past us by writing it as ::ffff:127.0.0.1.
func isBlocked(ip netip.Addr) bool {
	if !ip.IsValid() {
		// Defensive: an invalid address cannot be reached from the public
		// Internet, so refusing it is correct. This branch is exercised by
		// the "adapter returned a zero netip.Addr" test.
		return true
	}
	canonical := ip.Unmap()
	for _, p := range blockedPrefixes {
		if p.Contains(canonical) {
			return true
		}
	}
	return false
}
