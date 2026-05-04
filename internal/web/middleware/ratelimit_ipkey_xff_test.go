package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/ratelimit"
	"github.com/pericles-luz/crm/internal/ratelimit/adapter/memory"
	"github.com/pericles-luz/crm/internal/ratelimit/metrics"
	"github.com/pericles-luz/crm/internal/web/middleware"
)

// TestIPKey_DefaultDeniesXFF is the SIN-62287 unit regression: the
// package-level IPKey symbol must ignore X-Forwarded-For / X-Real-IP
// when no TrustedProxies are configured, even if the immediate peer
// looks plausible. The previous behaviour (trust the first XFF hop
// unconditionally) regressed the SIN-62167 / SIN-62177 policy and let
// an anonymous attacker bypass every IP-keyed bucket by rotating XFF.
func TestIPKey_DefaultDeniesXFF(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		xff        string
		realIP     string
		remoteAddr string
		want       string
		wantOK     bool
	}{
		{
			name:       "xff set, peer untrusted, returns remote addr host",
			xff:        "1.2.3.4, 5.6.7.8",
			remoteAddr: "10.0.0.1:443",
			want:       "10.0.0.1",
			wantOK:     true,
		},
		{
			name:       "x-real-ip set, peer untrusted, returns remote addr host",
			realIP:     "1.2.3.4",
			remoteAddr: "10.0.0.1:443",
			want:       "10.0.0.1",
			wantOK:     true,
		},
		{
			name:       "no forwarded headers, returns remote addr host",
			remoteAddr: "203.0.113.10:80",
			want:       "203.0.113.10",
			wantOK:     true,
		},
		{
			name:       "no remote addr, no forwarded headers, ok=false",
			remoteAddr: "",
			want:       "",
			wantOK:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.realIP != "" {
				req.Header.Set("X-Real-IP", tc.realIP)
			}
			got, ok := middleware.IPKey(req)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("IPKey = (%q, %v); want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestIPKeyFrom_TrustedProxyXFFRightmost validates the four acceptance-
// criteria cases from SIN-62287:
//
//  1. XFF set, RemoteAddr NOT in TrustedProxies → returns RemoteAddr host.
//  2. XFF set, RemoteAddr in TrustedProxies → returns rightmost XFF hop.
//  3. XFF unset → returns RemoteAddr host.
//  4. Empty TrustedProxies → never reads XFF (default-deny).
func TestIPKeyFrom_TrustedProxyXFFRightmost(t *testing.T) {
	t.Parallel()
	trusted := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/24"),
		netip.MustParsePrefix("2001:db8::/32"),
	}

	cases := []struct {
		name       string
		opts       middleware.IPKeyOpts
		xff        string
		realIP     string
		remoteAddr string
		want       string
		wantOK     bool
	}{
		{
			name:       "1) xff set + peer NOT trusted -> remote addr host",
			opts:       middleware.IPKeyOpts{TrustedProxies: trusted},
			xff:        "203.0.113.10, 198.51.100.7",
			remoteAddr: "192.0.2.5:1234",
			want:       "192.0.2.5",
			wantOK:     true,
		},
		{
			name:       "2) xff set + peer trusted -> rightmost xff hop",
			opts:       middleware.IPKeyOpts{TrustedProxies: trusted},
			xff:        "203.0.113.10, 198.51.100.7",
			remoteAddr: "10.0.0.42:1234",
			want:       "198.51.100.7",
			wantOK:     true,
		},
		{
			name:       "2b) single-entry xff + peer trusted -> that entry",
			opts:       middleware.IPKeyOpts{TrustedProxies: trusted},
			xff:        "203.0.113.10",
			remoteAddr: "10.0.0.5:443",
			want:       "203.0.113.10",
			wantOK:     true,
		},
		{
			name:       "2c) trailing whitespace + peer trusted -> trimmed rightmost hop",
			opts:       middleware.IPKeyOpts{TrustedProxies: trusted},
			xff:        "203.0.113.10,  198.51.100.7  ",
			remoteAddr: "10.0.0.42:1234",
			want:       "198.51.100.7",
			wantOK:     true,
		},
		{
			name:       "2d) trusted IPv6 peer -> rightmost xff hop",
			opts:       middleware.IPKeyOpts{TrustedProxies: trusted},
			xff:        "203.0.113.10, 198.51.100.7",
			remoteAddr: "[2001:db8::1]:443",
			want:       "198.51.100.7",
			wantOK:     true,
		},
		{
			name:       "2e) x-real-ip fallback when xff absent + peer trusted",
			opts:       middleware.IPKeyOpts{TrustedProxies: trusted},
			realIP:     "203.0.113.99",
			remoteAddr: "10.0.0.7:443",
			want:       "203.0.113.99",
			wantOK:     true,
		},
		{
			name:       "3) xff unset -> remote addr host (peer trusted, irrelevant)",
			opts:       middleware.IPKeyOpts{TrustedProxies: trusted},
			remoteAddr: "10.0.0.42:1234",
			want:       "10.0.0.42",
			wantOK:     true,
		},
		{
			name:       "4) empty TrustedProxies + xff set -> remote addr host",
			opts:       middleware.IPKeyOpts{},
			xff:        "203.0.113.10",
			remoteAddr: "10.0.0.42:1234",
			want:       "10.0.0.42",
			wantOK:     true,
		},
		{
			name:       "4b) empty TrustedProxies + x-real-ip set -> remote addr host",
			opts:       middleware.IPKeyOpts{},
			realIP:     "203.0.113.10",
			remoteAddr: "10.0.0.42:1234",
			want:       "10.0.0.42",
			wantOK:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			extract := middleware.IPKeyFrom(tc.opts)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.realIP != "" {
				req.Header.Set("X-Real-IP", tc.realIP)
			}
			got, ok := extract(req)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("extract = (%q, %v); want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestIPKeyFrom_NonIPRemoteAddrIsNeverTrusted ensures hostnames or
// unparseable peer addresses cannot be promoted to "trusted" by accident
// (e.g. someone configuring a CIDR that overlaps a domain literal).
func TestIPKeyFrom_NonIPRemoteAddrIsNeverTrusted(t *testing.T) {
	t.Parallel()
	extract := middleware.IPKeyFrom(middleware.IPKeyOpts{
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")},
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "internal.proxy.local:443"
	req.Header.Set("X-Forwarded-For", "203.0.113.10")
	got, ok := extract(req)
	if !ok || got != "internal.proxy.local" {
		t.Fatalf("hostnames must not be CIDR-trusted; got (%q, %v) want (\"internal.proxy.local\", true)", got, ok)
	}
}

// TestIPKeyFrom_EmptyXFFFromTrustedPeerFallsBackToRemoteAddr — guards
// against an off-by-one where a trusted peer sending XFF: "" leaks the
// peer address but is never returned (because rightmostXFF returns
// empty, we must continue to RemoteAddr fallback).
func TestIPKeyFrom_EmptyXFFFromTrustedPeerFallsBackToRemoteAddr(t *testing.T) {
	t.Parallel()
	extract := middleware.IPKeyFrom(middleware.IPKeyOpts{
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:443"
	req.Header.Set("X-Forwarded-For", "")
	got, ok := extract(req)
	if !ok || got != "10.0.0.5" {
		t.Fatalf("empty XFF must fall back to peer host; got (%q, %v)", got, ok)
	}
}

// TestApply_XFFRotationCannotBypassLoginIPBucket is the SIN-62287
// integration regression: an attacker rotating X-Forwarded-For per
// request from an untrusted peer must not be able to escape the 5/min
// IP-keyed bucket on POST /login. The 6th request from the same peer
// MUST 429, regardless of XFF gymnastics.
func TestApply_XFFRotationCannotBypassLoginIPBucket(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	lim := memory.New(memory.WithClock(clock.Now))
	rec := metrics.NewCounter()
	mw := middleware.Apply(lim, []middleware.Rule{
		{
			Endpoint:   "POST /login",
			Bucket:     "ip",
			Limit:      ratelimit.Limit{Window: time.Minute, Max: 5},
			Key:        middleware.IPKey, // default-deny — peer is the bucket
			FailClosed: true,
		},
	}, middleware.Config{Now: clock.Now, Metrics: rec, Logger: discardLogger()})
	h := mw(nextOK)

	rotatedXFFs := []string{
		"1.2.3.4",
		"2.3.4.5",
		"3.4.5.6",
		"4.5.6.7",
		"5.6.7.8",
		"6.7.8.9",
	}
	codes := make([]int, 0, len(rotatedXFFs))
	for _, xff := range rotatedXFFs {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(""))
		req.Header.Set("X-Forwarded-For", xff)
		req.RemoteAddr = "203.0.113.250:55555" // attacker-controlled peer
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		codes = append(codes, w.Code)
	}
	wantCodes := []int{200, 200, 200, 200, 200, http.StatusTooManyRequests}
	for i, want := range wantCodes {
		if codes[i] != want {
			t.Fatalf("request %d (xff=%s): code = %d, want %d (XFF rotation must NOT bypass IP bucket)",
				i+1, rotatedXFFs[i], codes[i], want)
		}
	}
	if got := rec.AllowedCount("POST /login", "ip"); got != 5 {
		t.Fatalf("Allowed = %d, want 5", got)
	}
	if got := rec.DeniedCount("POST /login", "ip"); got != 1 {
		t.Fatalf("Denied = %d, want 1", got)
	}
}

// TestApply_TrustedProxyConsultsRightmostXFF mirrors the production
// wiring: when cmd/server passes the front-door proxy CIDR list, the
// per-real-client IP bucket actually buckets per-real-client. With XFF
// "<attacker-poison>, <our-proxy's-record>" arriving from a trusted
// peer, the rightmost hop (the trusted proxy's record of the real
// client just before its own rewrite) is the bucket value, and the
// leftmost attacker-supplied entry is ignored.
func TestApply_TrustedProxyConsultsRightmostXFF(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	lim := memory.New(memory.WithClock(clock.Now))
	rec := metrics.NewCounter()
	trusted := []netip.Prefix{netip.MustParsePrefix("172.20.0.0/16")}
	mw := middleware.Apply(lim, []middleware.Rule{
		{
			Endpoint:   "POST /login",
			Bucket:     "ip",
			Limit:      ratelimit.Limit{Window: time.Minute, Max: 5},
			Key:        middleware.IPKeyFrom(middleware.IPKeyOpts{TrustedProxies: trusted}),
			FailClosed: true,
		},
	}, middleware.Config{Now: clock.Now, Metrics: rec, Logger: discardLogger()})
	h := mw(nextOK)

	// 5 requests from the same real client. XFF arrives shaped as
	// "<attacker-poison>, <real-client-as-seen-by-trusted-proxy>" —
	// leftmost is the attacker-controlled origin (RFC 7239), rightmost
	// is what survives the trusted-proxy rewrite. The extractor reads
	// rightmost, so the attacker cannot poison the bucket value.
	for i := 1; i <= 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(""))
		req.RemoteAddr = "172.20.0.10:443" // trusted Caddy peer
		req.Header.Set("X-Forwarded-For", "203.0.113.99, 192.0.2.7")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("req %d: code = %d, want 200", i, w.Code)
		}
	}
	// 6th request: rightmost stays "192.0.2.7" → same bucket → 429.
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(""))
	req.RemoteAddr = "172.20.0.10:443"
	req.Header.Set("X-Forwarded-For", "203.0.113.99, 192.0.2.7")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("6th request: code = %d, want 429", w.Code)
	}
}
