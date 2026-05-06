package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTokenBucketLimiter_FirstAllowSecondDenyThenRefill verifies the
// fixture's in-memory limiter gives one token, denies the next call until
// the refill window elapses, then allows again. The cooldown UI flow under
// test depends on exactly this shape.
func TestTokenBucketLimiter_FirstAllowSecondDenyThenRefill(t *testing.T) {
	t.Parallel()
	cooldown := 50 * time.Millisecond
	lim := newTokenBucketLimiter(cooldown)

	now := time.Unix(1_700_000_000, 0)
	lim.nowFunc = func() time.Time { return now }

	allowed, retry, err := lim.Allow(context.Background(), "bucket", "key")
	if err != nil {
		t.Fatalf("first Allow err = %v", err)
	}
	if !allowed {
		t.Fatalf("first Allow allowed = false, want true")
	}
	if retry != 0 {
		t.Fatalf("first Allow retry = %v, want 0", retry)
	}

	allowed, retry, err = lim.Allow(context.Background(), "bucket", "key")
	if err != nil {
		t.Fatalf("second Allow err = %v", err)
	}
	if allowed {
		t.Fatalf("second Allow allowed = true, want false")
	}
	if retry < time.Second {
		t.Fatalf("second Allow retry = %v, want >= 1s (Retry-After floor)", retry)
	}

	now = now.Add(cooldown)
	allowed, _, err = lim.Allow(context.Background(), "bucket", "key")
	if err != nil {
		t.Fatalf("third Allow err = %v", err)
	}
	if !allowed {
		t.Fatalf("third Allow allowed = false, want true after refill")
	}
}

// TestTokenBucketLimiter_DefaultCooldownWhenZero ensures the constructor
// guards against a zero cooldown that would otherwise let every call
// through.
func TestTokenBucketLimiter_DefaultCooldownWhenZero(t *testing.T) {
	t.Parallel()
	lim := newTokenBucketLimiter(0)
	if lim.cooldown <= 0 {
		t.Fatalf("cooldown = %v, want positive default", lim.cooldown)
	}
}

// TestTokenBucketLimiter_DistinctKeysShareNoBucket protects the limiter's
// keying so the conv-bucket and user-bucket the production middleware
// invokes do not stomp each other.
func TestTokenBucketLimiter_DistinctKeysShareNoBucket(t *testing.T) {
	t.Parallel()
	lim := newTokenBucketLimiter(time.Second)

	for _, key := range []string{"a", "b"} {
		allowed, _, err := lim.Allow(context.Background(), "bucket", key)
		if err != nil {
			t.Fatalf("Allow(%q) err = %v", key, err)
		}
		if !allowed {
			t.Fatalf("Allow(%q) allowed = false, want true", key)
		}
	}
}

// TestBuildMux_HostPageIncludesAssetsAndLiveButtonSlot verifies the
// fixture's host HTML wires htmx, the production stylesheet, and the
// live-button slot the cooldown swap depends on.
func TestBuildMux_HostPageIncludesAssetsAndLiveButtonSlot(t *testing.T) {
	t.Parallel()
	mux := buildMux(newTokenBucketLimiter(time.Second), time.Second, "./testdata-static")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`href="/static/css/aipanel.css"`,
		`src="/static/vendor/htmx/2.0.9/htmx.min.js"`,
		`id="ai-panel-slot"`,
		`hx-get="/refresh"`,
		`id="manual-refresh"`,
		// htmx defaults to dropping 4xx/5xx responses; the host page MUST
		// opt-in to swap on 429/503 or the cooldown fragment never
		// reaches the DOM. Assert the opt-in stays wired here.
		`htmx:beforeSwap`,
		`status === 429`,
		`status === 503`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("host page missing %q\nbody: %s", want, body)
		}
	}
}

// TestBuildMux_RegenLiveResponse_AllowedReturnsLiveButton verifies the
// happy path: a single POST to /regen with a fresh limiter returns the
// live button HTML so the swap target stays stable.
func TestBuildMux_RegenLiveResponse_AllowedReturnsLiveButton(t *testing.T) {
	t.Parallel()
	mux := buildMux(newTokenBucketLimiter(time.Second), time.Second, "./testdata-static")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/regen", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="ai-panel-regenerate"`) {
		t.Fatalf("body missing live button id\nbody: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `class="ai-panel-regenerate"`) {
		t.Fatalf("body missing live button class\nbody: %s", rec.Body.String())
	}
}

// TestBuildMux_RegenSpamClickReturnsCooldownFragment verifies the
// rate-limit denial flow that drives the UI: second POST in the same
// cooldown window returns 429, sets Retry-After, and the body is the
// cooldown fragment with the matching swap target id.
func TestBuildMux_RegenSpamClickReturnsCooldownFragment(t *testing.T) {
	t.Parallel()
	mux := buildMux(newTokenBucketLimiter(2*time.Second), 2*time.Second, "./testdata-static")

	first := httptest.NewRecorder()
	mux.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/regen", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.Code)
	}

	second := httptest.NewRecorder()
	mux.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/regen", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429", second.Code)
	}
	retry := second.Header().Get("Retry-After")
	if retry == "" {
		t.Fatalf("missing Retry-After header on 429")
	}
	if n, err := strconv.Atoi(retry); err != nil || n < 1 {
		t.Fatalf("Retry-After = %q, want positive integer seconds", retry)
	}
	if second.Header().Get("X-RateLimit-Retry-After-Ms") == "" {
		t.Fatalf("missing X-RateLimit-Retry-After-Ms on 429")
	}
	body := second.Body.String()
	if !strings.Contains(body, `id="ai-panel-regenerate"`) {
		t.Errorf("cooldown fragment missing swap target id\nbody: %s", body)
	}
	if !strings.Contains(body, `class="ai-panel-cooldown"`) {
		t.Errorf("cooldown fragment missing class\nbody: %s", body)
	}
	if !strings.Contains(body, `disabled`) {
		t.Errorf("cooldown fragment must be disabled\nbody: %s", body)
	}
}

// TestBuildMux_RefreshReturnsLiveButton verifies that GET /refresh
// returns the live button regardless of bucket state, so the Playwright
// suite can recover the live UI after the cooldown window expires.
func TestBuildMux_RefreshReturnsLiveButton(t *testing.T) {
	t.Parallel()
	mux := buildMux(newTokenBucketLimiter(time.Second), time.Second, "./testdata-static")

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/refresh", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: status = %d, want 200", i, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `class="ai-panel-regenerate"`) {
			t.Errorf("call %d: body missing live button class\nbody: %s", i, rec.Body.String())
		}
	}
}

// TestStaticIdentity_ReturnsStableTuple guards the fixture's hard-coded
// identity. If this drifts, the cooldown buckets won't be shared across
// requests and the spam-click test loses its determinism.
func TestStaticIdentity_ReturnsStableTuple(t *testing.T) {
	t.Parallel()
	tenant, user, conv, err := staticIdentity(httptest.NewRequest(http.MethodPost, "/regen", nil))
	if err != nil {
		t.Fatalf("staticIdentity err = %v", err)
	}
	if tenant == "" || user == "" || conv == "" {
		t.Fatalf("identity tuple has empty field: %q %q %q", tenant, user, conv)
	}
}

// TestRegenContextEscapesAreSanePostHTMX is a paranoia check: the live
// button's hx-post attribute must remain a relative path so a hostile
// label/path can never escape into javascript: or absolute origins via
// html/template. Done here (not just in aipanel package) because the
// fixture is the smallest harness that wires LiveButton through the
// network round-trip.
func TestRegenContextEscapesAreSanePostHTMX(t *testing.T) {
	t.Parallel()
	mux := buildMux(newTokenBucketLimiter(time.Second), time.Second, "./testdata-static")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/refresh", nil))

	body := rec.Body.String()
	if !strings.Contains(body, `hx-post="/regen"`) {
		t.Fatalf("expected hx-post=\"/regen\" on live button; body: %s", body)
	}
	if strings.Contains(strings.ToLower(body), "javascript:") {
		t.Fatalf("body contained javascript: scheme, escape regression\nbody: %s", body)
	}
}

// TestTokenBucketLimiter_ContextNotRequired protects against a future
// regression that would force callers to thread a non-nil context — the
// production middleware passes r.Context() which is always non-nil but
// the unit test surface intentionally accepts background.
func TestTokenBucketLimiter_ContextNotRequired(t *testing.T) {
	t.Parallel()
	lim := newTokenBucketLimiter(time.Second)
	allowed, _, err := lim.Allow(context.Background(), "bucket", "key")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !allowed {
		t.Fatalf("first Allow allowed = false")
	}
}

// TestRegen_BodyIsHTMLContentType is a small invariant — the swap will not
// work if the response writes text/plain.
func TestRegen_BodyIsHTMLContentType(t *testing.T) {
	t.Parallel()
	mux := buildMux(newTokenBucketLimiter(time.Second), time.Second, "./testdata-static")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/regen", nil))

	got := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(got, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html prefix", got)
	}
}

// TestTestReset_ClearsBucketOnLoopback verifies the loopback-gated reset
// endpoint actually empties every bucket so the next call is admitted.
func TestTestReset_ClearsBucketOnLoopback(t *testing.T) {
	t.Parallel()
	mux := buildMux(newTokenBucketLimiter(2*time.Second), 2*time.Second, "./testdata-static")

	// Burn the only token.
	first := httptest.NewRecorder()
	mux.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/regen", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("seed status = %d, want 200", first.Code)
	}

	// Confirm the bucket is empty.
	denied := httptest.NewRecorder()
	mux.ServeHTTP(denied, httptest.NewRequest(http.MethodPost, "/regen", nil))
	if denied.Code != http.StatusTooManyRequests {
		t.Fatalf("denied status = %d, want 429", denied.Code)
	}

	resetReq := httptest.NewRequest(http.MethodPost, "/test/reset", nil)
	resetReq.RemoteAddr = "127.0.0.1:54321"
	resetRec := httptest.NewRecorder()
	mux.ServeHTTP(resetRec, resetReq)
	if resetRec.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d, want 204", resetRec.Code)
	}

	again := httptest.NewRecorder()
	mux.ServeHTTP(again, httptest.NewRequest(http.MethodPost, "/regen", nil))
	if again.Code != http.StatusOK {
		t.Fatalf("post-reset status = %d, want 200", again.Code)
	}
}

// TestTestReset_RejectsNonLoopbackOrigins ensures the reset hook is only
// callable from loopback so a misconfigured -addr cannot expose it.
func TestTestReset_RejectsNonLoopbackOrigins(t *testing.T) {
	t.Parallel()
	mux := buildMux(newTokenBucketLimiter(time.Second), time.Second, "./testdata-static")

	req := httptest.NewRequest(http.MethodPost, "/test/reset", nil)
	req.RemoteAddr = "203.0.113.10:54321" // TEST-NET-3
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("reset from non-loopback status = %d, want 403", rec.Code)
	}
}

// TestIsLoopback exercises the loopback parser against the address
// shapes Go's net/http hands us: IPv4:port, [IPv6]:port, and unix.
func TestIsLoopback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"127.5.6.7:0", true},
		{"[::1]:8080", true},
		{"203.0.113.10:80", false},
		{"[2001:db8::1]:80", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isLoopback(tc.addr); got != tc.want {
			t.Errorf("isLoopback(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestRegen_IgnoresRequestBody(t *testing.T) {
	t.Parallel()
	mux := buildMux(newTokenBucketLimiter(time.Second), time.Second, "./testdata-static")

	req := httptest.NewRequest(http.MethodPost, "/regen", io.NopCloser(strings.NewReader("anything")))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestRun_StartsServerAndShutsDownCleanlyOnContextCancel exercises the
// real bind / serve / context-cancel / shutdown loop. We pin the listener
// to :0 so the OS picks a free port and resolve it via a quick HTTP
// probe before cancelling.
func TestRun_StartsServerAndShutsDownCleanlyOnContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := pickFreeAddr(t)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-addr", addr, "-cooldown", "100ms", "-static", "./testdata-static"})
	}()

	if err := waitForListener(addr, 2*time.Second); err != nil {
		t.Fatalf("server never came up: %v", err)
	}

	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET / err = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("run did not return after context cancel")
	}
}

// TestRun_ReturnsErrorWhenAddrAlreadyInUse exercises the listen-error
// path so the bind failure surfaces back to the caller (and we don't
// silently log + ignore).
func TestRun_ReturnsErrorWhenAddrAlreadyInUse(t *testing.T) {
	t.Parallel()

	addr := pickFreeAddr(t)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("preparing listener: %v", err)
	}
	defer listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = run(ctx, []string{"-addr", listener.Addr().String(), "-static", "./testdata-static"})
	if err == nil {
		t.Fatalf("run returned nil, want bind error")
	}
}

// TestRun_FlagParseErrorPropagates ensures a malformed flag set is
// surfaced rather than panicking the binary.
func TestRun_FlagParseErrorPropagates(t *testing.T) {
	t.Parallel()
	err := run(context.Background(), []string{"-bogus-flag"})
	if err == nil {
		t.Fatalf("run returned nil, want flag parse error")
	}
}

func pickFreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

func waitForListener(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("timeout waiting for listener")
}
