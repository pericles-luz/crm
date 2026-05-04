package middleware_test

// Tests for the SIN-62288 hardening items: HMAC log hash + secret rotation
// hook, HeaderKey length cap, splitHostPort behaviour on bare IPv6, NUL
// delimiter in composed keys, and ctx propagation into logUnavailable.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/ratelimit"
	"github.com/pericles-luz/crm/internal/web/middleware"
)

// captureKeyLimiter records the limiter key the middleware composed for
// each request, so tests can assert the on-wire layout (NUL delimiter, no
// `:` ambiguity) without exposing composeKey directly.
type captureKeyLimiter struct {
	keys []string
}

func (c *captureKeyLimiter) Check(_ context.Context, key string, lim ratelimit.Limit) (ratelimit.Decision, error) {
	c.keys = append(c.keys, key)
	return ratelimit.Decision{Allowed: true, Remaining: lim.Max - 1, Retry: 0}, nil
}

func TestComposeKey_UsesNULDelimiter(t *testing.T) {
	t.Parallel()
	lim := &captureKeyLimiter{}
	mw := middleware.Apply(lim, []middleware.Rule{
		{
			Endpoint: "POST /login",
			Bucket:   "ip",
			Limit:    ratelimit.Limit{Window: time.Minute, Max: 5},
			Key:      middleware.IPKey,
		},
	}, middleware.Config{Logger: slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	// IPv6 address contains ':' — the prior delimiter — so it stresses the
	// new NUL-based layout where neither operator nor attacker input can
	// collide with the segment separator.
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "[2001:db8::1]:443"
	h.ServeHTTP(httptest.NewRecorder(), req)

	if len(lim.keys) != 1 {
		t.Fatalf("limiter key count = %d, want 1", len(lim.keys))
	}
	got := lim.keys[0]
	parts := strings.Split(got, "\x00")
	if len(parts) != 3 {
		t.Fatalf("expected 3 NUL-separated segments, got %d in %q", len(parts), got)
	}
	if parts[0] != "POST_/login" {
		t.Fatalf("endpoint segment = %q, want POST_/login", parts[0])
	}
	if parts[1] != "ip" {
		t.Fatalf("bucket segment = %q, want ip", parts[1])
	}
	if parts[2] != "2001:db8::1" {
		t.Fatalf("value segment = %q, want 2001:db8::1 (':' must remain intact in value)", parts[2])
	}
	if strings.Contains(got, ":") && !strings.Contains(parts[2], ":") {
		t.Fatalf("IPv6 colons must live only inside the value segment; got %q", got)
	}
}

// trippedLimiter denies the very first request so we can drive the denied
// log line and read the bucket_value_hash field.
type trippedLimiter struct{}

func (trippedLimiter) Check(_ context.Context, _ string, lim ratelimit.Limit) (ratelimit.Decision, error) {
	return ratelimit.Decision{Allowed: false, Remaining: 0, Retry: time.Second}, nil
}

func denyHashFromLog(t *testing.T, secret []byte) string {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	mw := middleware.Apply(trippedLimiter{}, []middleware.Rule{
		{
			Endpoint: "POST /login",
			Bucket:   "ip",
			Limit:    ratelimit.Limit{Window: time.Minute, Max: 1},
			Key:      middleware.IPKey,
		},
	}, middleware.Config{Logger: logger, LogHashSecret: secret})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	h.ServeHTTP(httptest.NewRecorder(), req)

	var rec struct {
		BucketValueHash string `json:"bucket_value_hash"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("decode log: %v\nraw=%q", err, buf.String())
	}
	if rec.BucketValueHash == "" {
		t.Fatalf("bucket_value_hash missing in log: %q", buf.String())
	}
	return rec.BucketValueHash
}

func TestApply_HMACHash_DiffersFromRawSHA256(t *testing.T) {
	t.Parallel()
	secret := bytes.Repeat([]byte{0xAB}, 32)
	got := denyHashFromLog(t, secret)

	// Raw SHA-256/8B of the same value — the format used before SIN-62288.
	sum := sha256.Sum256([]byte("203.0.113.10"))
	naive := hex.EncodeToString(sum[:8])

	if got == naive {
		t.Fatalf("hash equals plain SHA-256/8B (%q); HMAC keying not in effect", got)
	}
}

func TestApply_HMACHash_StableForSameSecret(t *testing.T) {
	t.Parallel()
	secret := []byte("a-stable-server-secret")
	first := denyHashFromLog(t, secret)
	second := denyHashFromLog(t, secret)

	if first != second {
		t.Fatalf("same secret must give same hash; got %q and %q", first, second)
	}
}

func TestApply_HMACHash_DiffersBetweenInstancesWhenSecretIsRandom(t *testing.T) {
	t.Parallel()
	a := denyHashFromLog(t, nil)
	b := denyHashFromLog(t, nil)
	// With nil secrets the middleware fabricates a 32-byte random secret
	// per process invocation. Two independent Apply calls thus produce
	// different digests for the same input — proving the random fallback
	// is wired and not silently leaking a hard-coded key.
	if a == b {
		t.Fatalf("random secrets should yield different hashes; both were %q", a)
	}
}

func TestApply_HMACHash_DiffersBetweenSecrets(t *testing.T) {
	t.Parallel()
	a := denyHashFromLog(t, []byte("secret-A"))
	b := denyHashFromLog(t, []byte("secret-B"))
	if a == b {
		t.Fatalf("different secrets must produce different hashes; both were %q", a)
	}
}

func TestHeaderKey_RejectsOversizeValues(t *testing.T) {
	t.Parallel()
	extract := middleware.HeaderKey("X-Session-ID")
	t.Run("at limit accepted", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Session-ID", strings.Repeat("a", 256))
		got, ok := extract(req)
		if !ok {
			t.Fatal("256-byte value must be accepted")
		}
		if len(got) != 256 {
			t.Fatalf("len = %d, want 256", len(got))
		}
	})
	t.Run("over limit skipped", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Session-ID", strings.Repeat("a", 257))
		if _, ok := extract(req); ok {
			t.Fatal("257-byte value must be rejected to bound limiter cardinality")
		}
	})
	t.Run("padded value within limit after trim accepted", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		// Trimming reduces this to 250 chars — well under the cap.
		req.Header.Set("X-Session-ID", "   "+strings.Repeat("z", 250)+"   ")
		got, ok := extract(req)
		if !ok || len(got) != 250 {
			t.Fatalf("trimmed-and-under-cap = (%d, %v); want (250, true)", len(got), ok)
		}
	})
}

func TestApply_HeaderKey_OverLimitSkipsRule(t *testing.T) {
	t.Parallel()
	// End-to-end: an attacker shipping huge X-Session-ID values must not
	// inflate the limiter's distinct-key set. With the cap in place the
	// rule is skipped, the request reaches the handler, and the limiter
	// observes zero requests.
	lim := &captureKeyLimiter{}
	mw := middleware.Apply(lim, []middleware.Rule{
		{
			Endpoint: "GET /api/contacts",
			Bucket:   "session",
			Limit:    ratelimit.Limit{Window: time.Minute, Max: 60},
			Key:      middleware.HeaderKey("X-Session-ID"),
		},
	}, middleware.Config{Logger: slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))})

	delivered := 0
	h := mw(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { delivered++ }))

	req := httptest.NewRequest(http.MethodGet, "/api/contacts", nil)
	req.Header.Set("X-Session-ID", strings.Repeat("x", 4096))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if delivered != 1 {
		t.Fatalf("handler invocations = %d, want 1 (rule should be skipped, not 429)", delivered)
	}
	if len(lim.keys) != 0 {
		t.Fatalf("limiter consulted with key %q; rule must be skipped before composeKey runs", lim.keys[0])
	}
}

func TestIPKey_BareIPv6FromRemoteAddr(t *testing.T) {
	t.Parallel()
	// "::1" is bare IPv6, no port, no brackets — exactly what the previous
	// LastIndexByte split mangled into ":" / "1".
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "::1"
	got, ok := middleware.IPKey(req)
	if !ok {
		t.Fatal("IPKey must accept bare IPv6 RemoteAddr")
	}
	if got != "::1" {
		t.Fatalf("IPKey = %q, want %q (raw addr after AddrError fallback)", got, "::1")
	}
}

func TestIPKey_BracketedIPv6WithPort(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "[2001:db8::1]:443"
	got, ok := middleware.IPKey(req)
	if !ok {
		t.Fatal("IPKey must accept bracketed IPv6 with port")
	}
	if got != "2001:db8::1" {
		t.Fatalf("IPKey = %q, want %q (net.SplitHostPort on bracketed form)", got, "2001:db8::1")
	}
	// Sanity check: the helper should match net.SplitHostPort exactly on
	// well-formed inputs so we do not silently regress.
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil || host != got {
		t.Fatalf("stdlib disagrees: net.SplitHostPort(%q) = (%q, _, %v)", req.RemoteAddr, host, err)
	}
}

// ctxMarkerKey is the unexported context key used by the propagation test.
type ctxMarkerKey struct{}

// ctxAwareHandler is a slog.Handler that records the value of ctxMarkerKey
// on every Handle call. It lets us prove that ctx propagated end-to-end
// from r.Context() into the limiter-unavailable log line, which is
// otherwise invisible to JSONHandler/TextHandler (they ignore ctx).
type ctxAwareHandler struct {
	seen []string
}

func (*ctxAwareHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *ctxAwareHandler) Handle(ctx context.Context, _ slog.Record) error {
	v, _ := ctx.Value(ctxMarkerKey{}).(string)
	h.seen = append(h.seen, v)
	return nil
}
func (h *ctxAwareHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *ctxAwareHandler) WithGroup(_ string) slog.Handler      { return h }

type unavailableLimiter struct{}

func (unavailableLimiter) Check(context.Context, string, ratelimit.Limit) (ratelimit.Decision, error) {
	return ratelimit.Decision{}, ratelimit.ErrUnavailable
}

func TestApply_LogUnavailable_PropagatesRequestContext(t *testing.T) {
	t.Parallel()
	handler := &ctxAwareHandler{}
	logger := slog.New(handler)

	mw := middleware.Apply(unavailableLimiter{}, []middleware.Rule{
		{
			Endpoint:   "POST /login",
			Bucket:     "ip",
			Limit:      ratelimit.Limit{Window: time.Minute, Max: 5},
			Key:        middleware.IPKey,
			FailClosed: true,
		},
	}, middleware.Config{Logger: logger})
	h := mw(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))

	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	req = req.WithContext(context.WithValue(req.Context(), ctxMarkerKey{}, "trace-abc-123"))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if len(handler.seen) == 0 {
		t.Fatal("logUnavailable must produce at least one log record")
	}
	for i, marker := range handler.seen {
		if marker != "trace-abc-123" {
			t.Fatalf("record %d ctx marker = %q, want %q (logUnavailable dropped request ctx)", i, marker, "trace-abc-123")
		}
	}
}

func TestCSP_SetsCacheControlNoStore(t *testing.T) {
	t.Parallel()
	mw := middleware.CSP()
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	got := rec.Header().Get("Cache-Control")
	if got != "no-store" {
		t.Fatalf("Cache-Control = %q, want %q (CSP nonce responses must not be cached)", got, "no-store")
	}
}
