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
//   - Logging uses log/slog with the bucket value hashed (SHA-256 / 16 hex)
//     so we never persist PII in our log pipeline.
package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pericles-luz/crm/internal/ratelimit"
	"github.com/pericles-luz/crm/internal/ratelimit/metrics"
)

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
					logUnavailable(logger, rule, value, err)
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
					logger.Error("ratelimit: limiter rejected the call",
						slog.String("endpoint", rule.Endpoint),
						slog.String("bucket_name", rule.Bucket),
						slog.String("bucket_value_hash", hashValue(value)),
						slog.String("error", err.Error()),
					)
					writeUnavailable(w)
					return
				}
				if !dec.Allowed {
					recorder.Denied(rule.Endpoint, rule.Bucket)
					logger.Info("ratelimit: denied",
						slog.String("endpoint", rule.Endpoint),
						slog.String("bucket_name", rule.Bucket),
						slog.String("bucket_value_hash", hashValue(value)),
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

// composeKey assembles the bucket key as `endpoint:bucket:value`. Adapters
// receive this opaque string; only the middleware understands the layout.
//
// The endpoint label may contain a method+path (e.g. "POST /login"), so we
// normalize whitespace so the key never carries ambiguous separators.
func composeKey(endpoint, bucket, value string) string {
	endpoint = strings.ReplaceAll(endpoint, " ", "_")
	return endpoint + ":" + bucket + ":" + value
}

// hashValue returns the first 8 bytes of SHA-256 of value as hex. We keep
// only 64 bits of identity so logs cannot be reversed to the original PII
// (email, IP, tenant id) while still being useful for incident triage.
func hashValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
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

func logUnavailable(logger *slog.Logger, rule Rule, value string, err error) {
	level := slog.LevelWarn
	if rule.FailClosed {
		level = slog.LevelError
	}
	logger.Log(context.Background(), level, "ratelimit: backend unavailable",
		slog.String("endpoint", rule.Endpoint),
		slog.String("bucket_name", rule.Bucket),
		slog.String("bucket_value_hash", hashValue(value)),
		slog.Bool("fail_closed", rule.FailClosed),
		slog.String("error", err.Error()),
	)
}

// IPKey extracts the request's IP address. It prefers the first hop in the
// X-Forwarded-For chain (Caddy or any other trusted proxy is responsible
// for sanitising the header before we see it) and falls back to
// RemoteAddr.
func IPKey(r *http.Request) (string, bool) {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i]), true
		}
		return strings.TrimSpace(xff), true
	}
	if r.RemoteAddr == "" {
		return "", false
	}
	if host, _, err := splitHostPort(r.RemoteAddr); err == nil && host != "" {
		return host, true
	}
	return r.RemoteAddr, true
}

// FormFieldKey extracts a form field, normalised to lowercase + trimmed. It
// returns ok=false if the field is missing/empty so the rule is skipped
// (the upstream handler will reject the empty value with a 4xx).
func FormFieldKey(name string) KeyExtractor {
	return func(r *http.Request) (string, bool) {
		// We do not call r.ParseForm directly because some endpoints
		// stream JSON; FormValue is the read-only path that handles
		// both POST forms and query strings.
		if v := strings.TrimSpace(r.FormValue(name)); v != "" {
			return strings.ToLower(v), true
		}
		return "", false
	}
}

// HeaderKey extracts a header value (trimmed). Skips when the header is
// absent.
func HeaderKey(name string) KeyExtractor {
	return func(r *http.Request) (string, bool) {
		if v := strings.TrimSpace(r.Header.Get(name)); v != "" {
			return v, true
		}
		return "", false
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

// splitHostPort is like net.SplitHostPort, but tolerates RemoteAddr values
// that have no port (some test transports populate just an IP).
func splitHostPort(addr string) (string, string, error) {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return addr, "", nil
	}
	return addr[:i], addr[i+1:], nil
}
