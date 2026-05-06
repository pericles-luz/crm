// Package miekg is the production adapter for iam.dnsresolver.Resolver,
// using github.com/miekg/dns directly. ADR 0079 §1 forbids using
// net.Resolver because Go's stdlib resolver:
//
//   - silently drops AAAA when /etc/nsswitch.conf or cgo paths kick in;
//   - does not surface the AD bit (DNSSEC observability is a hard
//     requirement of ADR 0079 §2);
//   - is harder to point at the Unbound sidecar that protects the host
//     (see infra/caddy/unbound.conf).
//
// Concurrency: a Resolver is safe for concurrent use. The miekg client is
// re-used across calls; per-call work copies the small *dns.Msg.
package miekg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/pericles-luz/crm/internal/iam/dnsresolver"
)

// DefaultDialTimeout caps the time spent on a single UDP+TCP exchange. The
// caller's context still wins, but this gives us a safe ceiling when a
// caller passes context.Background().
const DefaultDialTimeout = 4 * time.Second

// Config tunes the adapter. Server is the Unbound sidecar address (typically
// "127.0.0.1:5353" inside the Caddy compose stack); see ADR 0079 §2.
//
// EnableDNSSEC controls whether queries set the DO+CD bits and whether we
// trust the AD bit on responses. Disabling it makes
// IPAnswer.VerifiedWithDNSSEC always false; production MUST set true.
type Config struct {
	Server       string
	DialTimeout  time.Duration
	EnableDNSSEC bool
}

// Resolver is the adapter type. NewResolver builds one wired to a Config.
type Resolver struct {
	cfg    Config
	client *dns.Client
}

// NewResolver constructs a Resolver. An empty Server defaults to the Unix
// stdlib resolver address (127.0.0.1:53), but production MUST pass the
// Unbound sidecar so HTTP-01 challenges in Caddy never see a private IP.
func NewResolver(cfg Config) *Resolver {
	if cfg.Server == "" {
		cfg.Server = "127.0.0.1:53"
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = DefaultDialTimeout
	}
	c := &dns.Client{
		Net:          "udp",
		Timeout:      cfg.DialTimeout,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  cfg.DialTimeout,
		WriteTimeout: cfg.DialTimeout,
	}
	return &Resolver{cfg: cfg, client: c}
}

// LookupIP issues an A query then an AAAA query and concatenates the
// results. We do BOTH queries unconditionally because some operators
// publish only AAAA — and from the SSRF perspective the AAAA path is
// strictly required (e.g. ::1 must be blocked).
//
// The function intentionally does NOT use net.Resolver.LookupIPAddr; see
// the package doc for why.
func (r *Resolver) LookupIP(ctx context.Context, host string) ([]dnsresolver.IPAnswer, error) {
	host = ensureFQDN(host)
	a, errA := r.queryIP(ctx, host, dns.TypeA)
	aaaa, errAAAA := r.queryIP(ctx, host, dns.TypeAAAA)

	switch {
	case errA != nil && errAAAA != nil:
		// Prefer NXDOMAIN over NOERROR so callers see "domain doesn't
		// exist" instead of the more confusing "no record".
		if errors.Is(errA, dnsresolver.ErrNotFound) || errors.Is(errAAAA, dnsresolver.ErrNotFound) {
			return nil, dnsresolver.ErrNotFound
		}
		return nil, errA
	case errA != nil && !errors.Is(errA, dnsresolver.ErrNoRecord):
		return nil, errA
	case errAAAA != nil && !errors.Is(errAAAA, dnsresolver.ErrNoRecord):
		return nil, errAAAA
	}

	merged := make([]dnsresolver.IPAnswer, 0, len(a)+len(aaaa))
	merged = append(merged, a...)
	merged = append(merged, aaaa...)
	if len(merged) == 0 {
		return nil, dnsresolver.ErrNoRecord
	}
	return merged, nil
}

// LookupTXT returns the concatenated character-string of each TXT RR. The
// concatenation rule (RFC 1035 §3.3.14) is "join all <character-string>s
// of one RR with no separator"; that is what miekg's TXT.Txt slice gives
// us via strings.Join.
func (r *Resolver) LookupTXT(ctx context.Context, host string) ([]string, error) {
	host = ensureFQDN(host)
	msg, err := r.exchange(ctx, host, dns.TypeTXT)
	if err != nil {
		return nil, err
	}
	switch msg.Rcode {
	case dns.RcodeNameError:
		return nil, dnsresolver.ErrNotFound
	case dns.RcodeSuccess:
	default:
		return nil, fmt.Errorf("miekg: TXT query for %q: rcode %s", host, dns.RcodeToString[msg.Rcode])
	}
	out := make([]string, 0, len(msg.Answer))
	for _, rr := range msg.Answer {
		t, ok := rr.(*dns.TXT)
		if !ok {
			continue
		}
		out = append(out, strings.Join(t.Txt, ""))
	}
	if len(out) == 0 {
		return nil, dnsresolver.ErrNoRecord
	}
	return out, nil
}

func (r *Resolver) queryIP(ctx context.Context, host string, qtype uint16) ([]dnsresolver.IPAnswer, error) {
	msg, err := r.exchange(ctx, host, qtype)
	if err != nil {
		return nil, err
	}
	switch msg.Rcode {
	case dns.RcodeNameError:
		return nil, dnsresolver.ErrNotFound
	case dns.RcodeSuccess:
	default:
		return nil, fmt.Errorf("miekg: %s query for %q: rcode %s", dns.TypeToString[qtype], host, dns.RcodeToString[msg.Rcode])
	}
	dnssec := r.cfg.EnableDNSSEC && msg.AuthenticatedData
	out := make([]dnsresolver.IPAnswer, 0, len(msg.Answer))
	for _, rr := range msg.Answer {
		switch v := rr.(type) {
		case *dns.A:
			if addr, ok := netip.AddrFromSlice(v.A.To4()); ok {
				out = append(out, dnsresolver.IPAnswer{IP: addr, VerifiedWithDNSSEC: dnssec})
			}
		case *dns.AAAA:
			if addr, ok := netip.AddrFromSlice(v.AAAA.To16()); ok {
				out = append(out, dnsresolver.IPAnswer{IP: addr, VerifiedWithDNSSEC: dnssec})
			}
		}
	}
	if len(out) == 0 {
		return nil, dnsresolver.ErrNoRecord
	}
	return out, nil
}

// exchange runs a single DNS query honouring the caller's context deadline.
// Errors from the underlying transport are mapped to dnsresolver.ErrTimeout
// when they look like deadlines, so callers can branch on errors.Is.
func (r *Resolver) exchange(ctx context.Context, host string, qtype uint16) (*dns.Msg, error) {
	m := new(dns.Msg)
	m.SetQuestion(host, qtype)
	if r.cfg.EnableDNSSEC {
		m.SetEdns0(4096, true)
		m.CheckingDisabled = false
	}

	// Honour the caller's deadline by deriving one for the exchange.
	deadline, _ := ctx.Deadline()
	timeout := r.cfg.DialTimeout
	if !deadline.IsZero() {
		if d := time.Until(deadline); d > 0 && d < timeout {
			timeout = d
		}
	}
	c := *r.client // shallow copy so we can adjust per-call timeout safely.
	c.Timeout = timeout
	c.DialTimeout = timeout
	c.ReadTimeout = timeout
	c.WriteTimeout = timeout

	resp, _, err := c.ExchangeContext(ctx, m, r.cfg.Server)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, dnsresolver.ErrTimeout
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, dnsresolver.ErrTimeout
		}
		return nil, fmt.Errorf("miekg: exchange %s %s: %w", dns.TypeToString[qtype], host, err)
	}
	return resp, nil
}

// ensureFQDN appends a trailing dot if the caller did not, because miekg
// requires fully-qualified names. We also lowercase to keep cache
// behaviour deterministic.
func ensureFQDN(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if !strings.HasSuffix(host, ".") {
		host += "."
	}
	return host
}
