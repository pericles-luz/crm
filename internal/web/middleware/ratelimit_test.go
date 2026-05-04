package middleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/ratelimit"
	"github.com/pericles-luz/crm/internal/ratelimit/adapter/memory"
	"github.com/pericles-luz/crm/internal/ratelimit/metrics"
	"github.com/pericles-luz/crm/internal/web/middleware"
)

// fakeClock is a deterministic clock used in tests that need a stable
// X-RateLimit-Reset header value.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// failingLimiter always returns an error wrapping ratelimit.ErrUnavailable.
// We use it to exercise fail-open and fail-closed paths.
type failingLimiter struct{ err error }

func (l failingLimiter) Check(_ context.Context, _ string, _ ratelimit.Limit) (ratelimit.Decision, error) {
	return ratelimit.Decision{}, l.err
}

// invalidLimiter returns a non-ErrUnavailable error, mimicking a
// configuration mistake (e.g. zero Limit) that should fail-closed.
type invalidLimiter struct{}

func (invalidLimiter) Check(context.Context, string, ratelimit.Limit) (ratelimit.Decision, error) {
	return ratelimit.Decision{}, errors.New("misconfigured")
}

// nextOK is a tiny handler that signals "request reached the protected
// route" by writing a 200 with body "ok".
var nextOK = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, "ok")
})

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestApply_PassesThroughBelowLimit(t *testing.T) {
	t.Parallel()
	lim := memory.New()
	mw := middleware.Apply(lim, []middleware.Rule{
		{Endpoint: "POST /login", Bucket: "ip", Limit: ratelimit.Limit{Window: time.Minute, Max: 5}, Key: middleware.IPKey},
	}, middleware.Config{Logger: discardLogger()})
	h := mw(nextOK)

	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "203.0.113.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q, want ok", rec.Body.String())
	}
	if rec.Header().Get("X-RateLimit-Bypass") != "" {
		t.Fatalf("bypass header should not be set on success path")
	}
}

func TestApply_LoginIPRegression_SixthRequestIs429(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	lim := memory.New(memory.WithClock(clock.Now))
	rec := metrics.NewCounter()
	mw := middleware.Apply(lim, []middleware.Rule{
		{
			Endpoint:   "POST /login",
			Bucket:     "ip",
			Limit:      ratelimit.Limit{Window: time.Minute, Max: 5},
			Key:        middleware.IPKey,
			FailClosed: true,
		},
	}, middleware.Config{Now: clock.Now, Metrics: rec, Logger: discardLogger()})
	h := mw(nextOK)

	for i := 1; i <= 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.RemoteAddr = "203.0.113.10:9090"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200", i, w.Code)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "203.0.113.10:9090"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("6th status = %d, want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got == "" {
		t.Fatal("Retry-After header missing on 429")
	}
	retryAfter, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || retryAfter <= 0 {
		t.Fatalf("Retry-After must be positive int, got %q", w.Header().Get("Retry-After"))
	}
	if got := w.Header().Get("X-RateLimit-Limit"); got != "5" {
		t.Fatalf("X-RateLimit-Limit = %q, want 5", got)
	}
	if got := w.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 0", got)
	}
	reset, err := strconv.ParseInt(w.Header().Get("X-RateLimit-Reset"), 10, 64)
	if err != nil || reset <= clock.Now().Unix() {
		t.Fatalf("X-RateLimit-Reset = %q, want unix-epoch > now", w.Header().Get("X-RateLimit-Reset"))
	}

	var body struct {
		Error             string `json:"error"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body decode: %v", err)
	}
	if body.Error != "rate_limited" {
		t.Fatalf("body.error = %q, want rate_limited", body.Error)
	}
	if body.RetryAfterSeconds <= 0 {
		t.Fatalf("body.retry_after_seconds = %d, want > 0", body.RetryAfterSeconds)
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "ip") || strings.Contains(strings.ToLower(w.Body.String()), "bucket") {
		t.Fatalf("body must be anti-enum (no bucket name); got %q", w.Body.String())
	}

	if got := rec.AllowedCount("POST /login", "ip"); got != 5 {
		t.Fatalf("metrics Allowed = %d, want 5", got)
	}
	if got := rec.DeniedCount("POST /login", "ip"); got != 1 {
		t.Fatalf("metrics Denied = %d, want 1", got)
	}
}

func TestApply_LoginEmailRegression_EleventhRequestIs429(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	lim := memory.New(memory.WithClock(clock.Now))
	mw := middleware.Apply(lim, []middleware.Rule{
		{
			Endpoint: "POST /login",
			Bucket:   "email",
			Limit:    ratelimit.Limit{Window: time.Hour, Max: 10},
			Key:      middleware.FormFieldKey("email"),
		},
	}, middleware.Config{Now: clock.Now, Logger: discardLogger()})
	h := mw(nextOK)

	for i := 1; i <= 10; i++ {
		body := strings.NewReader("email=victim%40example.com")
		req := httptest.NewRequest(http.MethodPost, "/login", body)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200", i, w.Code)
		}
	}
	body := strings.NewReader("email=victim%40example.com")
	req := httptest.NewRequest(http.MethodPost, "/login", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("11th status = %d, want 429", w.Code)
	}
}

func TestApply_PasswordResetRegression_FourthIs429EvenForUnknownEmail(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	lim := memory.New(memory.WithClock(clock.Now))
	mw := middleware.Apply(lim, []middleware.Rule{
		{
			Endpoint: "POST /password/reset",
			Bucket:   "email",
			Limit:    ratelimit.Limit{Window: time.Hour, Max: 3},
			Key:      middleware.FormFieldKey("email"),
		},
	}, middleware.Config{Now: clock.Now, Logger: discardLogger()})
	h := mw(nextOK)

	for i := 1; i <= 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/password/reset",
			strings.NewReader("email=unknown%40example.com"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200 (counter must increment even for unknown email)", i, w.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/password/reset",
		strings.NewReader("email=unknown%40example.com"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("4th status = %d, want 429", w.Code)
	}
}

type tenantCtxKey struct{}

func TestApply_LGPDExportRegression_SecondCallSameDayIs429(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	lim := memory.New(memory.WithClock(clock.Now))
	mw := middleware.Apply(lim, []middleware.Rule{
		{
			Endpoint:   "POST /lgpd/export",
			Bucket:     "tenant_id",
			Limit:      ratelimit.Limit{Window: 24 * time.Hour, Max: 1},
			Key:        middleware.ContextValueKey(tenantCtxKey{}),
			FailClosed: true,
		},
	}, middleware.Config{Now: clock.Now, Logger: discardLogger()})
	h := mw(nextOK)

	withTenant := func(tenant string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/lgpd/export", nil)
		ctx := context.WithValue(req.Context(), tenantCtxKey{}, tenant)
		return req.WithContext(ctx)
	}

	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, withTenant("acme"))
	if w1.Code != http.StatusOK {
		t.Fatalf("1st export status = %d, want 200", w1.Code)
	}

	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, withTenant("acme"))
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("2nd export status = %d, want 429", w2.Code)
	}

	// Different tenant should NOT be affected.
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, withTenant("globex"))
	if w3.Code != http.StatusOK {
		t.Fatalf("globex tenant first call status = %d, want 200 (independent bucket)", w3.Code)
	}
}

func TestApply_FailClosed_503OnLimiterUnavailable(t *testing.T) {
	t.Parallel()
	rec := metrics.NewCounter()
	mw := middleware.Apply(failingLimiter{err: ratelimit.ErrUnavailable}, []middleware.Rule{
		{
			Endpoint:   "POST /login",
			Bucket:     "ip",
			Limit:      ratelimit.Limit{Window: time.Minute, Max: 5},
			Key:        middleware.IPKey,
			FailClosed: true,
		},
	}, middleware.Config{Metrics: rec, Logger: discardLogger()})
	h := mw(nextOK)

	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if got := rec.UnavailableCount("POST /login"); got != 1 {
		t.Fatalf("Unavailable counter = %d, want 1", got)
	}
}

func TestApply_FailOpen_AllowsAndSetsBypassHeader(t *testing.T) {
	t.Parallel()
	rec := metrics.NewCounter()
	mw := middleware.Apply(failingLimiter{err: ratelimit.ErrUnavailable}, []middleware.Rule{
		{
			Endpoint: "GET /api/contacts",
			Bucket:   "user_id",
			Limit:    ratelimit.Limit{Window: time.Minute, Max: 60},
			Key:      middleware.HeaderKey("X-User-ID"),
			// FailClosed: false → fail-open
		},
	}, middleware.Config{Metrics: rec, Logger: discardLogger()})
	h := mw(nextOK)

	req := httptest.NewRequest(http.MethodGet, "/api/contacts", nil)
	req.Header.Set("X-User-ID", "u-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open path)", w.Code)
	}
	if got := w.Header().Get("X-RateLimit-Bypass"); got != "redis-unavailable" {
		t.Fatalf("X-RateLimit-Bypass = %q, want redis-unavailable", got)
	}
	if got := rec.UnavailableCount("GET /api/contacts"); got != 1 {
		t.Fatalf("Unavailable counter = %d, want 1", got)
	}
}

func TestApply_DisabledByConfig_IsPassThrough(t *testing.T) {
	t.Parallel()
	disabled := false
	mw := middleware.Apply(memory.New(), []middleware.Rule{
		{Endpoint: "x", Bucket: "y", Limit: ratelimit.Limit{Window: time.Second, Max: 1}, Key: middleware.IPKey},
	}, middleware.Config{Enabled: &disabled, Logger: discardLogger()})
	h := mw(nextOK)

	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:5"
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("disabled middleware must pass through; got %d", w.Code)
		}
	}
}

func TestApply_NoRules_IsPassThrough(t *testing.T) {
	t.Parallel()
	mw := middleware.Apply(memory.New(), nil, middleware.Config{Logger: discardLogger()})
	h := mw(nextOK)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("empty rules must pass through; got %d", w.Code)
	}
}

func TestApply_FirstRuleDeniesShortCircuits(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	lim := memory.New(memory.WithClock(clock.Now))
	rec := metrics.NewCounter()
	mw := middleware.Apply(lim, []middleware.Rule{
		{Endpoint: "POST /login", Bucket: "ip", Limit: ratelimit.Limit{Window: time.Minute, Max: 1}, Key: middleware.IPKey},
		{Endpoint: "POST /login", Bucket: "email", Limit: ratelimit.Limit{Window: time.Hour, Max: 10}, Key: middleware.FormFieldKey("email")},
	}, middleware.Config{Now: clock.Now, Metrics: rec, Logger: discardLogger()})
	h := mw(nextOK)

	for i := 1; i <= 1; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=v%40example.com"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "1.1.1.1:1"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
	}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=v%40example.com"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "1.1.1.1:1"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after first rule denial, got %d", w.Code)
	}
	// Second rule must NOT have been consulted (no Allowed for email bucket
	// on this request).
	if got := rec.AllowedCount("POST /login", "email"); got != 1 {
		t.Fatalf("email rule must be evaluated only on the allowed first request; got %d allowed", got)
	}
}

func TestApply_MisconfiguredLimit_FailsClosed(t *testing.T) {
	t.Parallel()
	mw := middleware.Apply(invalidLimiter{}, []middleware.Rule{
		{Endpoint: "x", Bucket: "y", Limit: ratelimit.Limit{Window: time.Second, Max: 1}, Key: middleware.IPKey},
	}, middleware.Config{Logger: discardLogger()})
	h := mw(nextOK)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "1.2.3.4:5"
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("misconfiguration error must produce 503, got %d", w.Code)
	}
}

func TestApply_ExtractorOk_FalseSkipsRule(t *testing.T) {
	t.Parallel()
	called := false
	checker := middleware.KeyExtractor(func(_ *http.Request) (string, bool) {
		called = true
		return "", false
	})
	mw := middleware.Apply(memory.New(), []middleware.Rule{
		{Endpoint: "x", Bucket: "y", Limit: ratelimit.Limit{Window: time.Minute, Max: 1}, Key: checker},
	}, middleware.Config{Logger: discardLogger()})
	h := mw(nextOK)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("rule must be skipped when extractor returns ok=false; got %d", w.Code)
	}
	if !called {
		t.Fatal("extractor was not consulted")
	}
}

// --- KeyExtractor unit tests ---

// TestIPKey asserts the default-deny contract of the package-level
// IPKey symbol after SIN-62287. The two "x-forwarded-for ..." cases
// previously asserted that IPKey trusted X-Forwarded-For unconditionally
// (the regression of the SIN-62167 / SIN-62177 policy). They now assert
// the corrected default: with no TrustedProxies configured, IPKey MUST
// ignore the header and bucket per peer (RemoteAddr). The trusted-proxy
// and rightmost-hop scenarios live in
// TestIPKeyFrom_TrustedProxyXFFRightmost / TestIPKey_DefaultDeniesXFF.
func TestIPKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		xff    string
		remote string
		want   string
		wantOK bool
	}{
		{"x-forwarded-for ignored without trusted proxies (multi-hop)", "203.0.113.10, 10.0.0.1", "10.0.0.1:80", "10.0.0.1", true},
		{"x-forwarded-for ignored without trusted proxies (single hop)", "203.0.113.10", "10.0.0.1:80", "10.0.0.1", true},
		{"remoteaddr with port", "", "203.0.113.10:80", "203.0.113.10", true},
		{"remoteaddr no port", "", "203.0.113.10", "203.0.113.10", true},
		{"missing both", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remote
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			got, ok := middleware.IPKey(req)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("IPKey = (%q, %v); want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestFormFieldKey_NormalisesAndSkips(t *testing.T) {
	t.Parallel()
	extract := middleware.FormFieldKey("email")
	t.Run("normalises", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("email=Foo%40Example.COM"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		got, ok := extract(req)
		if !ok || got != "foo@example.com" {
			t.Fatalf("FormFieldKey = (%q, %v)", got, ok)
		}
	})
	t.Run("missing field skips", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("other=bar"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		_, ok := extract(req)
		if ok {
			t.Fatal("missing email field must yield ok=false")
		}
	})
}

// TestFormFieldKey_IgnoresQueryString is the SIN-62286 regression: an
// empty-body POST whose target field lives in the URL query string MUST
// be ignored by the extractor, so an attacker cannot trip the rate-limit
// bucket of an arbitrary victim email without sending a real form body.
func TestFormFieldKey_IgnoresQueryString(t *testing.T) {
	t.Parallel()
	extract := middleware.FormFieldKey("email")

	t.Run("post with empty body and query field skips", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodPost, "/login?email=victim%40example.com", nil)
		_, ok := extract(req)
		if ok {
			t.Fatal("query-string-only field MUST NOT be picked up; otherwise pre-auth lockout DoS is possible (SIN-62286)")
		}
	})

	t.Run("post with body present takes body, not query", func(t *testing.T) {
		t.Parallel()
		// Body says realuser; query string says attacker bait.
		req := httptest.NewRequest(http.MethodPost, "/login?email=spoof%40example.com",
			strings.NewReader("email=Real%40Example.com"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		got, ok := extract(req)
		if !ok {
			t.Fatal("body field present must yield ok=true")
		}
		if got != "real@example.com" {
			t.Fatalf("FormFieldKey = %q; want body value %q (NOT the query-string value)", got, "real@example.com")
		}
	})

	t.Run("get request with query field skips", func(t *testing.T) {
		t.Parallel()
		// PostFormValue only consults the body for POST/PUT/PATCH; GET
		// returns "" → ok=false. Email-keyed rules are wired only on
		// POST endpoints anyway.
		req := httptest.NewRequest(http.MethodGet, "/login?email=victim%40example.com", nil)
		_, ok := extract(req)
		if ok {
			t.Fatal("GET requests must not feed an email-keyed bucket via the query string")
		}
	})
}

// TestApply_EmptyBodyQueryStringDoesNotIncrementEmailBucket is the
// SIN-62286 integration regression: 50 empty-body POSTs whose only
// "email" lives in the URL query string MUST NOT count against the
// per-email bucket. A 51st request that *does* submit the email in the
// body must still be allowed (10/h budget intact).
func TestApply_EmptyBodyQueryStringDoesNotIncrementEmailBucket(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	lim := memory.New(memory.WithClock(clock.Now))
	rec := metrics.NewCounter()
	mw := middleware.Apply(lim, []middleware.Rule{
		{
			Endpoint:   "POST /login",
			Bucket:     "email",
			Limit:      ratelimit.Limit{Window: time.Hour, Max: 10},
			Key:        middleware.FormFieldKey("email"),
			FailClosed: true,
		},
	}, middleware.Config{Now: clock.Now, Metrics: rec, Logger: discardLogger()})
	h := mw(nextOK)

	// 50 empty-body forgeries — many times the 10/h budget. None must
	// land on the email bucket.
	for i := 0; i < 50; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login?email=victim%40example.com", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("forgery %d: status = %d, want 200 (rule must be skipped, not denied)", i, w.Code)
		}
	}
	if got := rec.AllowedCount("POST /login", "email"); got != 0 {
		t.Fatalf("query-string forgeries leaked into email bucket: Allowed=%d, want 0", got)
	}
	if got := rec.DeniedCount("POST /login", "email"); got != 0 {
		t.Fatalf("query-string forgeries triggered Denied=%d on email bucket; want 0", got)
	}

	// Now hit the same endpoint with the email *in the body*. The full
	// 10/h budget must still be available.
	for i := 1; i <= 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login?email=spoof%40example.com",
			strings.NewReader("email=victim%40example.com"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("legitimate body request %d: status = %d, want 200", i, w.Code)
		}
	}
	// 11th body request must trip 429 — confirms the bucket is intact
	// and the previous query-string traffic did not steal budget from
	// it.
	req := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader("email=victim%40example.com"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("11th body request status = %d, want 429 (bucket integrity check)", w.Code)
	}
}

func TestHeaderKey(t *testing.T) {
	t.Parallel()
	extract := middleware.HeaderKey("X-User-ID")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-User-ID", "  u-1  ")
	got, ok := extract(req)
	if !ok || got != "u-1" {
		t.Fatalf("HeaderKey = (%q, %v); want (u-1, true)", got, ok)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, ok := extract(req2); ok {
		t.Fatal("absent header must yield ok=false")
	}
}

type strKey string

func TestContextValueKey(t *testing.T) {
	t.Parallel()
	const k = strKey("user_id")
	extract := middleware.ContextValueKey(k)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), k, "u-1"))
	got, ok := extract(req)
	if !ok || got != "u-1" {
		t.Fatalf("ContextValueKey = (%q, %v); want (u-1, true)", got, ok)
	}

	if _, ok := extract(httptest.NewRequest(http.MethodGet, "/", nil)); ok {
		t.Fatal("absent context value must yield ok=false")
	}

	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	req3 = req3.WithContext(context.WithValue(req3.Context(), k, 42))
	if _, ok := extract(req3); ok {
		t.Fatal("non-string context value must yield ok=false")
	}

	req4 := httptest.NewRequest(http.MethodGet, "/", nil)
	req4 = req4.WithContext(context.WithValue(req4.Context(), k, ""))
	if _, ok := extract(req4); ok {
		t.Fatal("empty context value must yield ok=false")
	}
}
