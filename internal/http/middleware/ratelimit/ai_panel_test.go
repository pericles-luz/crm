package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	aiport "github.com/pericles-luz/crm/internal/ai/port"
)

// fakeLimiter is a script-driven port.RateLimiter. tests push (allowed,
// retry, err) tuples in the order they expect Allow to be invoked.
type fakeLimiter struct {
	results []limiterResult
	calls   []limiterCall
}

type limiterResult struct {
	allowed bool
	retry   time.Duration
	err     error
}

type limiterCall struct {
	bucket string
	key    string
}

func (f *fakeLimiter) Allow(_ context.Context, bucket, key string) (bool, time.Duration, error) {
	f.calls = append(f.calls, limiterCall{bucket, key})
	if len(f.calls) > len(f.results) {
		// default to allowed when test forgot to script — surfaces wiring bugs
		return true, 0, nil
	}
	r := f.results[len(f.calls)-1]
	return r.allowed, r.retry, r.err
}

func staticIdentity(t, u, c string) IdentityResolver {
	return func(*http.Request) (string, string, string, error) {
		return t, u, c, nil
	}
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func newRecorder() *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}

func TestMiddleware_Allowed_PassesThrough(t *testing.T) {
	resetMetricsForTest()
	lim := &fakeLimiter{
		results: []limiterResult{
			{allowed: true},
			{allowed: true},
		},
	}
	mw := Middleware(Config{Limiter: lim, Identity: staticIdentity("t", "u", "c")})
	rec := newRecorder()
	mw(okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ai/panel/regen", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "ok")
	}
	if len(lim.calls) != 2 {
		t.Fatalf("limiter calls = %d, want 2", len(lim.calls))
	}
	if lim.calls[0].bucket != BucketUserConv || lim.calls[0].key != "tenant:t:user:u:conv:c" {
		t.Fatalf("call[0] = %+v, want user-conv tenant:t:user:u:conv:c", lim.calls[0])
	}
	if lim.calls[1].bucket != BucketUser || lim.calls[1].key != "tenant:t:user:u" {
		t.Fatalf("call[1] = %+v, want user tenant:t:user:u", lim.calls[1])
	}
}

func TestMiddleware_TwoConvRequests_SecondReturns429(t *testing.T) {
	resetMetricsForTest()
	// First request: both allowed → 200.
	// Second request: user-conv denies (retry 30s) → 429 with Retry-After 30.
	lim := &fakeLimiter{
		results: []limiterResult{
			{allowed: true},
			{allowed: true},
			{allowed: false, retry: 30 * time.Second}, // user-conv on req 2
			{allowed: true}, // user on req 2
		},
	}
	mw := Middleware(Config{Limiter: lim, Identity: staticIdentity("t", "u", "c")})

	rec1 := newRecorder()
	mw(okHandler()).ServeHTTP(rec1, httptest.NewRequest(http.MethodPost, "/r", nil))
	if rec1.Code != http.StatusOK {
		t.Fatalf("req1 status = %d, want 200", rec1.Code)
	}

	rec2 := newRecorder()
	mw(okHandler()).ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/r", nil))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("req2 status = %d, want 429", rec2.Code)
	}
	if got := rec2.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want 30", got)
	}
	if got := rec2.Header().Get("X-RateLimit-Retry-After-Ms"); got != "30000" {
		t.Fatalf("X-RateLimit-Retry-After-Ms = %q, want 30000", got)
	}

	// Counter incremented for the user-conv bucket / quota reason.
	got := testutil.ToFloat64(rateLimitedTotal.WithLabelValues(BucketUserConv, "quota"))
	if got != 1 {
		t.Fatalf("counter[user-conv,quota] = %v, want 1", got)
	}
}

func TestMiddleware_EleventhUserRequest_Returns429(t *testing.T) {
	resetMetricsForTest()
	// Build script for 11 requests: first 10 allowed on both buckets,
	// 11th: user-conv allowed (different conv each time so OK), user denies.
	results := make([]limiterResult, 0, 22)
	for i := 0; i < 10; i++ {
		results = append(results,
			limiterResult{allowed: true},
			limiterResult{allowed: true},
		)
	}
	results = append(results,
		limiterResult{allowed: true},                          // user-conv ok on 11th
		limiterResult{allowed: false, retry: 6 * time.Second}, // user denies
	)
	lim := &fakeLimiter{results: results}
	mw := Middleware(Config{Limiter: lim, Identity: staticIdentity("t", "u", "c")})

	for i := 0; i < 10; i++ {
		rec := newRecorder()
		mw(okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/r", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("req %d status = %d, want 200", i+1, rec.Code)
		}
	}
	rec := newRecorder()
	mw(okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/r", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("req 11 status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "6" {
		t.Fatalf("Retry-After = %q, want 6", got)
	}

	got := testutil.ToFloat64(rateLimitedTotal.WithLabelValues(BucketUser, "quota"))
	if got != 1 {
		t.Fatalf("counter[user,quota] = %v, want 1", got)
	}
}

func TestMiddleware_RedisDown_FailsClosed_503(t *testing.T) {
	resetMetricsForTest()
	lim := &fakeLimiter{
		results: []limiterResult{
			{allowed: false, retry: 5 * time.Second, err: fmt.Errorf("dial tcp: %w", aiport.ErrLimiterUnavailable)},
			{allowed: false, retry: 5 * time.Second, err: fmt.Errorf("dial tcp: %w", aiport.ErrLimiterUnavailable)},
		},
	}
	mw := Middleware(Config{Limiter: lim, Identity: staticIdentity("t", "u", "c")})
	rec := newRecorder()
	mw(okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/r", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatalf("Retry-After missing")
	}
	if got := testutil.ToFloat64(rateLimitedTotal.WithLabelValues(BucketUserConv, "backend_unavailable")); got != 1 {
		t.Fatalf("counter[user-conv,backend_unavailable] = %v, want 1", got)
	}
}

func TestMiddleware_OnlyUserBucketUnavailable_FailsClosedWithUserLabel(t *testing.T) {
	resetMetricsForTest()
	lim := &fakeLimiter{
		results: []limiterResult{
			{allowed: true},
			{allowed: false, retry: 5 * time.Second, err: errors.Join(errors.New("redis error"), aiport.ErrLimiterUnavailable)},
		},
	}
	mw := Middleware(Config{Limiter: lim, Identity: staticIdentity("t", "u", "c")})
	rec := newRecorder()
	mw(okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/r", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := testutil.ToFloat64(rateLimitedTotal.WithLabelValues(BucketUser, "backend_unavailable")); got != 1 {
		t.Fatalf("counter[user,backend_unavailable] = %v, want 1", got)
	}
}

func TestMiddleware_BothDenied_RetryAfterIsMax(t *testing.T) {
	resetMetricsForTest()
	lim := &fakeLimiter{
		results: []limiterResult{
			{allowed: false, retry: 30 * time.Second},
			{allowed: false, retry: 6 * time.Second},
		},
	}
	mw := Middleware(Config{Limiter: lim, Identity: staticIdentity("t", "u", "c")})
	rec := newRecorder()
	mw(okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/r", nil))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want 30 (max of 30 and 6)", got)
	}
}

func TestMiddleware_UnauthorizedWhenIdentityErrors(t *testing.T) {
	resetMetricsForTest()
	identity := func(*http.Request) (string, string, string, error) {
		return "", "", "", errors.New("no auth")
	}
	mw := Middleware(Config{Limiter: &fakeLimiter{}, Identity: identity})
	rec := newRecorder()
	mw(okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/r", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestMiddleware_UnauthorizedWhenIdentityEmpty(t *testing.T) {
	resetMetricsForTest()
	identity := func(*http.Request) (string, string, string, error) {
		return "t", "", "c", nil
	}
	mw := Middleware(Config{Limiter: &fakeLimiter{}, Identity: identity})
	rec := newRecorder()
	mw(okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/r", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestMiddleware_PanicsWithoutLimiter(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when Limiter is nil")
		}
	}()
	Middleware(Config{Identity: staticIdentity("t", "u", "c")})
}

func TestMiddleware_PanicsWithoutIdentity(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when Identity is nil")
		}
	}()
	Middleware(Config{Limiter: &fakeLimiter{}})
}

func TestMiddleware_CustomRendererCalled(t *testing.T) {
	resetMetricsForTest()
	called := false
	render := func(w http.ResponseWriter, _ *http.Request, retry time.Duration, reason string) {
		called = true
		if retry != 30*time.Second {
			t.Errorf("renderer retry = %v, want 30s", retry)
		}
		if reason != "quota" {
			t.Errorf("renderer reason = %q, want quota", reason)
		}
		_, _ = w.Write([]byte("custom"))
	}
	lim := &fakeLimiter{
		results: []limiterResult{
			{allowed: false, retry: 30 * time.Second},
			{allowed: true},
		},
	}
	mw := Middleware(Config{Limiter: lim, Identity: staticIdentity("t", "u", "c"), Render: render})
	rec := newRecorder()
	mw(okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/r", nil))
	if !called {
		t.Fatal("custom renderer not invoked")
	}
	if rec.Body.String() != "custom" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "custom")
	}
}

func TestIdentityFromContext_Roundtrip(t *testing.T) {
	t.Parallel()
	type ctxKey struct{}
	get := func(ctx context.Context) (string, string, string, error) {
		v, _ := ctx.Value(ctxKey{}).(string)
		if v == "" {
			return "", "", "", errors.New("no identity")
		}
		return "t", "u", v, nil
	}
	resolver := IdentityFromContext(get)

	req := httptest.NewRequest(http.MethodGet, "/r", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, "conv-42"))
	tn, u, c, err := resolver(req)
	if err != nil || tn != "t" || u != "u" || c != "conv-42" {
		t.Fatalf("resolver = (%q,%q,%q,%v), want (t,u,conv-42,nil)", tn, u, c, err)
	}
}

func TestMustRegister_Idempotent(t *testing.T) {
	resetMetricsForTest()
	reg := prometheus.NewRegistry()
	if err := MustRegister(reg); err != nil {
		t.Fatalf("first MustRegister err = %v", err)
	}
	// Second call must not error or re-register.
	if err := MustRegister(reg); err != nil {
		t.Fatalf("second MustRegister err = %v", err)
	}
}

func TestDefaultRenderer_BackendUnavailableMessage(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	defaultRenderer(rec, httptest.NewRequest(http.MethodGet, "/r", nil), 5*time.Second, "backend_unavailable")
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("body empty")
	}
}

func TestWriteRetryAfter_RoundsUp(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	writeRetryAfter(rec, 1100*time.Millisecond)
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want 2 (1.1s rounds up)", got)
	}
	if got := rec.Header().Get("X-RateLimit-Retry-After-Ms"); got != "1100" {
		t.Fatalf("X-RateLimit-Retry-After-Ms = %q, want 1100", got)
	}
}

func TestWriteRetryAfter_ClampsToOne(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	writeRetryAfter(rec, 100*time.Millisecond)
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1 (clamped)", got)
	}
}
