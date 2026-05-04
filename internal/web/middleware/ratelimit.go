// Package middleware contains HTTP middlewares wired around the rate-limit
// and security-header ports defined in ADR 0081 / 0082 (SIN-62245).
//
// Wiring policy:
//
//   - Apply produces an http.Handler middleware that consults a ratelimit.Limiter
//     for every configured Rule. Multiple rules per route are evaluated in
//     declaration order; the first denial short-circuits with 429.
//   - The 429 body is anti-enumeration: it never names the bucket that
//     tripped, only the retry budget.
//   - On limiter failure (Redis down, etc.) each Rule decides between
//     fail-closed (503) and fail-open (allow + X-RateLimit-Bypass header).
//   - Logging uses log/slog with the bucket value HMAC-hashed (HMAC-SHA256
//     truncated to 8 bytes / 16 hex chars) using a process secret. The
//     resulting digest is reversible *only* by an attacker who already
//     holds the server secret; treat it as pseudonymised PII subject to
//     the same retention/deletion policies (LGPD), not as fully anonymous
//     data. Secret rotation is governed by ADR 0073 (planned, SIN-62199).
package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/pericles-luz/crm/internal/ratelimit"
	"github.com/pericles-luz/crm/internal/ratelimit/metrics"
)

// keyDelimiter separates the endpoint, bucket and value segments inside a
// limiter key. NUL is unreachable in HTTP header field values, e-mail
// local-parts, and IP textual forms, so it cannot collide with operator-
// or attacker-controlled content. See SIN-62288 item 4.
const keyDelimiter = "\x00"

// maxHeaderKeyValueLen caps how many bytes HeaderKey accepts from an
// inbound header before declaring the bucket value invalid. Without a cap
// a pre-auth attacker can spam millions of distinct header values to
// inflate Redis bucket cardinality (memory pressure / ops cost). 256 is
// well above any legitimate session ID, JWT or correlation ID we issue.
// See SIN-62288 item 2.
const maxHeaderKeyValueLen = 256

// logHashSecretBytes is the byte length of the per-process HMAC key used
// to hash bucket values for structured logs. 32 bytes (256 bits) matches
// the SHA-256 block-equivalent strength.
const logHashSecretBytes = 32

// KeyExtractor pulls the bucket value out of an inbound request. Returning
// ok=false skips the rule for this request (e.g. an email bucket on a
// request that has no email form field).
type KeyExtractor func(r *http.Request) (value string, ok bool)

// Rule describes one bucket evaluated by the middleware.
type Rule struct {
	// Endpoint is the human label that appears in metrics and logs. By
	// convention, "METHOD /path" (e.g. "POST /login").
	Endpoint string
	// Bucket is the label that identifies which bucket inside Endpoint a
	// request belongs to (e.g. "ip", "email"). Surfaced in metrics; never
	// surfaced in the 429 body (anti-enumeration).
	Bucket string
	// Limit is the budget for the bucket per ADR 0081 §1.
	Limit ratelimit.Limit
	// Key extracts the bucket value from the request.
	Key KeyExtractor
	// FailClosed makes the middleware return 503 when the limiter cannot
	// be consulted (default: fail-open with X-RateLimit-Bypass). ADR 0081
	// §5 mandates fail-closed for /login, /2fa/verify, /password/reset,
	// /lgpd/export and master ops.
	FailClosed bool
}

// Config tunes the middleware. Zero values are safe defaults: the
// middleware is enabled, uses time.Now and slog.Default, and emits no
// metrics.
type Config struct {
	// Enabled toggles the entire middleware off when false. Maps to the
	// global `RATELIMIT_ENABLED` env var per ADR 0081 §"Reversibilidade".
	Enabled *bool
	// Now is the clock used to compute X-RateLimit-Reset. Tests pass a
	// frozen clock to make header values deterministic.
	Now func() time.Time
	// Logger is the slog logger used for the structured rate-limit log
	// line. nil → slog.Default().
	Logger *slog.Logger
	// Metrics is the Recorder for the three Prometheus counters
	// described in ADR 0081 §6. nil → no-op.
	Metrics metrics.Recorder
	// LogHashSecret keys the HMAC used to derive bucket_value_hash. When
	// empty Apply generates a 32-byte random secret at construction
	// time, scoped to the process. Production deployments should supply
	// a stable secret loaded from the environment so log entries are
	// correlatable across instances; rotation is governed by ADR 0073
	// (planned, SIN-62199).
	LogHashSecret []byte
}

// logHasher hashes bucket values for structured logs using HMAC-SHA256
// keyed by a process-scoped or operator-supplied secret. The 8-byte
// truncated digest is reversible only by attackers who already hold the
// secret; the goal is pseudonymisation, not anonymisation.
type logHasher struct {
	secret []byte
}

func (h logHasher) hash(value string) string {
	mac := hmac.New(sha256.New, h.secret)
	mac.Write([]byte(value))
	sum := mac.Sum(nil)
	return hex.EncodeToString(sum[:8])
}

// newLogHasher returns a logHasher seeded with secret, or with 32 random
// bytes when secret is empty. crypto/rand failure during construction is
// fatal so the middleware never falls back to a predictable digest.
func newLogHasher(secret []byte) logHasher {
	if len(secret) > 0 {
		// Defensive copy so callers can scrub the source slice without
		// changing observed log digests.
		buf := make([]byte, len(secret))
		copy(buf, secret)
		return logHasher{secret: buf}
	}
	buf := make([]byte, logHashSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("ratelimit middleware: generate log hash secret: %v", err))
	}
	return logHasher{secret: buf}
}

// Apply returns an http.Handler middleware that evaluates rules against
// limiter on every incoming request. The slice may be empty, in which case
// the middleware is a transparent pass-through (used so callers can wire
// the middleware into their router unconditionally and turn it on later).
func Apply(limiter ratelimit.Limiter, rules []Rule, cfg Config) func(http.Handler) http.Handler {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	recorder := cfg.Metrics
	if recorder == nil {
		recorder = metrics.Noop{}
	}
	enabled := true
	if cfg.Enabled != nil {
		enabled = *cfg.Enabled
	}
	hasher := newLogHasher(cfg.LogHashSecret)

	return func(next http.Handler) http.Handler {
		if !enabled || len(rules) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, rule := range rules {
				value, ok := rule.Key(r)
				if !ok {
					continue
				}
				key := composeKey(rule.Endpoint, rule.Bucket, value)
				dec, err := limiter.Check(r.Context(), key, rule.Limit)
				switch {
				case errors.Is(err, ratelimit.ErrUnavailable):
					recorder.Unavailable(rule.Endpoint)
					logUnavailable(r.Context(), logger, hasher, rule, value, err)
					if rule.FailClosed {
						writeUnavailable(w)
						return
					}
					// Fail-open: signal bypass and let the request through.
					w.Header().Set("X-RateLimit-Bypass", "redis-unavailable")
					continue
				case err != nil:
					// Misconfiguration (e.g. zero Limit) is fail-closed by
					// definition: the operator wants to know.
					recorder.Unavailable(rule.Endpoint)
					logger.LogAttrs(r.Context(), slog.LevelError, "ratelimit: limiter rejected the call",
						slog.String("endpoint", rule.Endpoint),
						slog.String("bucket_name", rule.Bucket),
						slog.String("bucket_value_hash", hasher.hash(value)),
						slog.String("error", err.Error()),
					)
					writeUnavailable(w)
					return
				}
				if !dec.Allowed {
					recorder.Denied(rule.Endpoint, rule.Bucket)
					logger.LogAttrs(r.Context(), slog.LevelInfo, "ratelimit: denied",
						slog.String("endpoint", rule.Endpoint),
						slog.String("bucket_name", rule.Bucket),
						slog.String("bucket_value_hash", hasher.hash(value)),
						slog.Int("limit", rule.Limit.Max),
						slog.Duration("retry", dec.Retry),
					)
					writeDenied(w, rule.Limit.Max, dec.Retry, now())
					return
				}
				recorder.Allowed(rule.Endpoint, rule.Bucket)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// composeKey assembles the bucket key as `endpoint<NUL>bucket<NUL>value`.
// Adapters receive this opaque string; only the middleware understands the
// layout. NUL is impossible in any of the three segments (HTTP header
// values, e-mail addresses, IPv4/IPv6 textual forms, tenant IDs) so the
// segments cannot collide regardless of operator config or attacker input.
//
// The endpoint label may contain a method+path (e.g. "POST /login"), so we
// normalize whitespace so logs and metrics labels remain readable.
func composeKey(endpoint, bucket, value string) string {
	endpoint = strings.ReplaceAll(endpoint, " ", "_")
	return endpoint + keyDelimiter + bucket + keyDelimiter + value
}

func writeDenied(w http.ResponseWriter, limit int, retry time.Duration, now time.Time) {
	retrySeconds := int(math.Ceil(retry.Seconds()))
	if retrySeconds < 1 {
		retrySeconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
	w.Header().Set("X-RateLimit-Remaining", "0")
	resetEpoch := now.Add(time.Duration(retrySeconds) * time.Second).Unix()
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetEpoch, 10))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusTooManyRequests)
	body := struct {
		Error             string `json:"error"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	}{
		Error:             "rate_limited",
		RetryAfterSeconds: retrySeconds,
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = fmt.Fprintln(w, `{"error":"rate_limit_unavailable"}`)
}

// logUnavailable emits the warn/error line for a limiter-unavailable
// outcome. ctx must be the request context so request-scoped slog
// attributes (request id, trace id, tenant id) survive into the log line.
func logUnavailable(ctx context.Context, logger *slog.Logger, hasher logHasher, rule Rule, value string, err error) {
	level := slog.LevelWarn
	if rule.FailClosed {
		level = slog.LevelError
	}
	logger.LogAttrs(ctx, level, "ratelimit: backend unavailable",
		slog.String("endpoint", rule.Endpoint),
		slog.String("bucket_name", rule.Bucket),
		slog.String("bucket_value_hash", hasher.hash(value)),
		slog.Bool("fail_closed", rule.FailClosed),
		slog.String("error", err.Error()),
	)
}

// IPKeyOpts configures the IP extractor returned by IPKeyFrom.
//
// TrustedProxies is the allow-list of proxy peer addresses we are willing
// to consult X-Forwarded-For / X-Real-IP for. The check is against the
// host parsed out of r.RemoteAddr (the immediate TCP peer). When empty,
// the extractor never reads forwarded-IP headers — the policy is
// default-deny, mirroring the SIN-62167 / SIN-62177 decision for the
// legacy clientIP helper. cmd/server is responsible for opting in by
// passing the production CIDR list at wiring time (typically the
// front-door reverse-proxy network: Caddy peer, internal load balancer
// CIDRs).
type IPKeyOpts struct {
	TrustedProxies []netip.Prefix
}

// IPKey extracts the request's IP address with X-Forwarded-For default-
// denied. See IPKeyFrom for the full contract; this is IPKeyFrom called
// with a zero-value IPKeyOpts, so it never trusts XFF / X-Real-IP.
//
// Production wiring (cmd/server) must replace this default with
// IPKeyFrom(IPKeyOpts{TrustedProxies: <production CIDRs>}). The
// per-request, IP-keyed buckets in config/ratelimit.yaml ("POST /login"
// ip and "/api/*" ip) collapse to bucketing per Caddy peer otherwise.
//
// Why default-deny: trusting XFF unconditionally lets an anonymous
// remote attacker bypass every IP-keyed bucket by rotating the header
// value per request — the original defect class fixed in SIN-62167 and
// codified in SIN-62177. SIN-62287 re-applies that policy here.
func IPKey(r *http.Request) (string, bool) {
	return ipKeyFromCompiled(r, nil)
}

// IPKeyFrom returns a KeyExtractor that consults forwarded-IP headers
// only when r.RemoteAddr's host is contained in opts.TrustedProxies.
//
// Resolution order, when the peer is trusted:
//  1. The rightmost entry of X-Forwarded-For — that is the only entry
//     that survives a trusted proxy's rewrite of the header (RFC 7239,
//     Caddy and nginx convention). The leftmost entry is attacker-
//     controlled when XFF arrives on the wire.
//  2. X-Real-IP, as a single-value fallback emitted by some proxies.
//  3. The peer's own RemoteAddr host.
//
// When the peer is NOT trusted (or TrustedProxies is empty), the
// extractor ignores both headers and returns RemoteAddr's host. ok=false
// only when there is no usable address at all (RemoteAddr empty AND
// every other source unparseable).
func IPKeyFrom(opts IPKeyOpts) KeyExtractor {
	prefixes := append([]netip.Prefix(nil), opts.TrustedProxies...)
	return func(r *http.Request) (string, bool) {
		return ipKeyFromCompiled(r, prefixes)
	}
}

func ipKeyFromCompiled(r *http.Request, trusted []netip.Prefix) (string, bool) {
	peer := remoteAddrHost(r)
	if peer != "" && peerIsTrusted(peer, trusted) {
		if v := rightmostXFF(r.Header.Get("X-Forwarded-For")); v != "" {
			return v, true
		}
		if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
			return v, true
		}
	}
	if peer != "" {
		return peer, true
	}
	if r.RemoteAddr == "" {
		return "", false
	}
	return r.RemoteAddr, true
}

// remoteAddrHost extracts the host portion of r.RemoteAddr. It honours
// the bracketed-IPv6 form (e.g. "[2001:db8::1]:443" → "2001:db8::1") and
// tolerates RemoteAddr values that have no port (some test transports
// populate just an IP).
func remoteAddrHost(r *http.Request) string {
	if r.RemoteAddr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		return host
	}
	// No port (or unparseable host:port). Strip enclosing brackets if
	// present so a bracketed IPv6 literal still parses cleanly downstream.
	addr := r.RemoteAddr
	if len(addr) >= 2 && addr[0] == '[' && addr[len(addr)-1] == ']' {
		return addr[1 : len(addr)-1]
	}
	return addr
}

// peerIsTrusted reports whether host (parsed as an IP) sits in any of
// the configured TrustedProxies CIDRs. A non-IP host (e.g. unix socket
// or a hostname) is never trusted, by construction.
func peerIsTrusted(host string, trusted []netip.Prefix) bool {
	if len(trusted) == 0 {
		return false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// rightmostXFF returns the last (rightmost) hop in an X-Forwarded-For
// header, trimmed. Empty input returns the empty string. The rightmost
// entry is the only one that survives a trusted proxy's rewrite — every
// hop further left was supplied by something we are not authoritative
// over.
func rightmostXFF(xff string) string {
	if xff == "" {
		return ""
	}
	if i := strings.LastIndexByte(xff, ','); i >= 0 {
		return strings.TrimSpace(xff[i+1:])
	}
	return strings.TrimSpace(xff)
}

// FormFieldKey extracts a form field from the request body only,
// normalised to lowercase + trimmed. It returns ok=false if the field is
// missing/empty so the rule is skipped (the upstream handler will reject
// the empty value with a 4xx).
//
// Body-only by design (SIN-62286). We use r.PostFormValue, not
// r.FormValue, because r.FormValue merges the URL query string with the
// parsed body — that lets an unauthenticated attacker trip an arbitrary
// victim's per-email bucket with empty-body forgeries such as
// `POST /login?email=victim%40example.com`. Query-string sourced values
// MUST NOT increment email-keyed buckets. For POST/PUT/PATCH,
// r.PostFormValue consults only the body, which is the desired
// behaviour for the email rate-limit rules in config/ratelimit.yaml
// (`POST /login` and `POST /password/reset`).
//
// Body-consumption gotcha. r.PostFormValue triggers r.ParseMultipartForm
// (which calls r.ParseForm), and both read and consume r.Body for
// application/x-www-form-urlencoded and multipart/form-data requests.
// After this middleware runs, downstream handlers that try to read the
// raw body (e.g. json.NewDecoder(r.Body)) will see EOF. Recommended
// pattern for endpoints that need this rate-limit AND a non-form body:
// either (a) buffer-and-restore r.Body before this middleware (e.g.
// drain to a bytes.Buffer and re-attach via io.NopCloser), or (b) move
// the endpoint to application/x-www-form-urlencoded so the downstream
// handler reads the same parsed form via r.PostForm. The parsed values
// remain available on r.PostForm/r.Form after the body is consumed, so
// handlers that opt into form encoding pay no extra cost.
func FormFieldKey(name string) KeyExtractor {
	return func(r *http.Request) (string, bool) {
		// PostFormValue ignores the URL query string for POST/PUT/PATCH;
		// for other methods it returns the empty string, which yields
		// ok=false here. That is the correct behaviour — email-keyed
		// rate-limit rules only target body-bearing methods.
		if v := strings.TrimSpace(r.PostFormValue(name)); v != "" {
			return strings.ToLower(v), true
		}
		return "", false
	}
}

// HeaderKey extracts a header value (trimmed). It rejects values longer
// than maxHeaderKeyValueLen bytes (after trimming) to bound limiter-key
// cardinality: an unauthenticated attacker who controls the header can
// otherwise force unbounded distinct keys × TTL into the limiter store.
// The rule is skipped (ok=false) when the header is absent, empty, or
// over-long; the underlying handler should still validate the field
// itself for shape and authenticity.
func HeaderKey(name string) KeyExtractor {
	return func(r *http.Request) (string, bool) {
		v := strings.TrimSpace(r.Header.Get(name))
		if v == "" {
			return "", false
		}
		if len(v) > maxHeaderKeyValueLen {
			return "", false
		}
		return v, true
	}
}

// ContextValueKey reads an opaque value previously stored in the request
// context (e.g. by the auth middleware). The key argument is the typed
// context key used at write time; we use any here so middleware authors
// can supply whatever distinct type they prefer.
func ContextValueKey(ctxKey any) KeyExtractor {
	return func(r *http.Request) (string, bool) {
		v := r.Context().Value(ctxKey)
		if v == nil {
			return "", false
		}
		s, ok := v.(string)
		if !ok || s == "" {
			return "", false
		}
		return s, true
	}
}
