// Package main is the AI panel E2E fixture binary (SIN-62318).
//
// This is NOT a production server. It exists solely so the Playwright
// browser-smoke suite under tests/e2e/ can drive the live regenerate
// button → 429 cooldown swap → recovery flow against the real renderers
// (aipanel.LiveButton + aipanel.CooldownFragment) and the real rate-limit
// middleware (internal/http/middleware/ratelimit). The limiter backend is
// an in-memory token bucket with capacity 1 and a configurable refill so
// tests can observe denial and recovery deterministically inside a few
// seconds.
//
// Routes (all served on -addr, default 127.0.0.1:8088):
//
//	GET  /                  Host HTML page (htmx + aipanel.css + LiveButton)
//	GET  /static/...        Pass-through to ./web/static/
//	POST /regen             Behind the ratelimit middleware. On success
//	                        returns LiveButton so the swap target is stable
//	                        and the user can click again. On 429/503 the
//	                        middleware writes the cooldown fragment via
//	                        aipanel.CooldownRenderer.
//	GET  /refresh           Returns LiveButton unconditionally. Used by the
//	                        Playwright "manual refresh" affordance to verify
//	                        the live state is recoverable after a cooldown
//	                        window expires (the production cooldown fragment
//	                        does not auto-recover today — see SIN-62318
//	                        follow-up note).
//
// Identity is hard-coded so the middleware sees the same (tenant, user,
// conv) tuple on every request — that is what causes spam-clicks to share
// a bucket and trip the limiter.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	aiport "github.com/pericles-luz/crm/internal/ai/port"
	"github.com/pericles-luz/crm/internal/http/handler/aipanel"
	"github.com/pericles-luz/crm/internal/http/middleware/ratelimit"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Printf("aipanel-e2e-fixture: %v", err)
		os.Exit(1)
	}
}

// run is the testable entry point. It parses flags, builds the mux, and
// runs the server until ctx is cancelled. Exposed so a unit test can
// drive a real bind/serve/shutdown cycle without exec'ing the binary.
func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("aipanel-e2e-fixture", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8088", "listen address (loopback by default — fixture is not a public server)")
	cooldown := fs.Duration("cooldown", 2*time.Second, "token-bucket refill interval; same as Retry-After surfaced to the browser")
	staticDir := fs.String("static", "./web/static", "path to the web/static asset tree (htmx + aipanel.css)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	limiter := newTokenBucketLimiter(*cooldown)
	mux := buildMux(limiter, *cooldown, *staticDir)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("aipanel-e2e-fixture: listening on http://%s (cooldown=%s)", *addr, *cooldown)
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-serveErr; err != nil {
		return err
	}
	return nil
}

func buildMux(limiter aiport.RateLimiter, retryAfter time.Duration, staticDir string) *http.ServeMux {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir))))

	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(hostPage))
	})

	mux.HandleFunc("GET /refresh", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = aipanel.LiveButton(w, aipanel.LiveButtonOptions{PostPath: "/regen"})
	})

	regenHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = aipanel.LiveButton(w, aipanel.LiveButtonOptions{PostPath: "/regen"})
	})

	mw := ratelimit.Middleware(ratelimit.Config{
		Limiter:  limiter,
		Identity: staticIdentity,
		Render:   aipanel.CooldownRenderer,
	})

	mux.Handle("POST /regen", mw(regenHandler))

	// Test-only reset endpoint so the Playwright suite can reliably empty
	// the bucket between scenarios without sleeping for cooldown windows.
	// Loopback-only is enforced by the fact that the fixture itself binds
	// to 127.0.0.1 by default; this endpoint adds a defense-in-depth check
	// so a misconfigured -addr cannot turn the fixture into a public reset
	// hook.
	if r, ok := limiter.(resettableLimiter); ok {
		mux.HandleFunc("POST /test/reset", func(w http.ResponseWriter, req *http.Request) {
			if !isLoopback(req.RemoteAddr) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			r.Reset()
			w.WriteHeader(http.StatusNoContent)
		})
	}

	slog.Info("aipanel-e2e-fixture wired", "retry_after", retryAfter)
	return mux
}

// resettableLimiter is a tiny extension that the test reset hook requires.
// The token bucket implementation provides it; production limiters do not.
type resettableLimiter interface {
	aiport.RateLimiter
	Reset()
}

// isLoopback returns true when remoteAddr is on 127.0.0.0/8 or ::1. The
// fixture's bind address is loopback by default, but we belt-and-braces
// the test reset endpoint here so a misconfigured -addr (e.g. 0.0.0.0)
// does not expose Reset() to the world.
func isLoopback(remoteAddr string) bool {
	host := remoteAddr
	if i := lastIndexColon(remoteAddr); i >= 0 {
		host = remoteAddr[:i]
	}
	host = trimBrackets(host)
	if host == "::1" {
		return true
	}
	if len(host) >= 4 && host[:4] == "127." {
		return true
	}
	return false
}

func lastIndexColon(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

func trimBrackets(s string) string {
	if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
		return s[1 : len(s)-1]
	}
	return s
}

// staticIdentity returns the same tuple for every request so the limiter
// keys are stable across the spam-click flow.
func staticIdentity(*http.Request) (string, string, string, error) {
	return "tenant-e2e", "user-e2e", "conv-e2e", nil
}

// hostPage is the minimal HTML the Playwright suite drives. It pulls the
// vendored htmx bundle and the production aipanel.css, then renders the
// live button into the swap slot via a server-side hx-get on load. This
// keeps the Go side as the single source of truth for the button HTML.
//
// The inline <script> wires htmx:beforeSwap so the cooldown fragment
// (served with a 429 / 503 status) actually gets swapped into the DOM.
// htmx 2.x defaults to NOT swapping on 4xx/5xx responses, so any real
// host page that wants to render the AI panel cooldown UI MUST opt in
// here. The fixture documents the smallest correct opt-in for the two
// status codes the rate-limit middleware emits.
const hostPage = `<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>AI Panel cooldown E2E fixture (SIN-62318)</title>
  <link rel="stylesheet" href="/static/css/aipanel.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js"></script>
  <script>
    document.addEventListener('htmx:beforeSwap', function (evt) {
      var status = evt.detail && evt.detail.xhr && evt.detail.xhr.status;
      if (status === 429 || status === 503) {
        evt.detail.shouldSwap = true;
        evt.detail.isError = false;
      }
    });
  </script>
</head>
<body>
  <main id="panel-host">
    <h1>AI panel cooldown smoke</h1>
    <p>This page exists for the SIN-62318 Playwright suite. It is not a
       production surface.</p>
    <div id="ai-panel-slot"
         hx-get="/refresh"
         hx-trigger="load"
         hx-swap="innerHTML"></div>
    <p>
      <button id="manual-refresh"
              type="button"
              hx-get="/refresh"
              hx-target="#ai-panel-regenerate"
              hx-swap="outerHTML">
        Manual refresh
      </button>
    </p>
  </main>
</body>
</html>
`

// tokenBucketLimiter is a tiny in-memory port.RateLimiter for the fixture.
// Capacity is 1 token per (bucket,key) pair; one token refills every
// `cooldown`. The narrow capacity makes the spam-click flow deterministic:
// the first request succeeds, the second is denied with a precise
// retry-after the test can wait on.
type tokenBucketLimiter struct {
	cooldown time.Duration

	mu      sync.Mutex
	nextOK  map[string]time.Time
	nowFunc func() time.Time
}

func newTokenBucketLimiter(cooldown time.Duration) *tokenBucketLimiter {
	if cooldown <= 0 {
		cooldown = time.Second
	}
	return &tokenBucketLimiter{
		cooldown: cooldown,
		nextOK:   make(map[string]time.Time),
		nowFunc:  time.Now,
	}
}

// Reset clears every bucket so the next Allow on any (bucket,key) is
// admitted. Test-only: the production middleware never calls this.
func (l *tokenBucketLimiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextOK = make(map[string]time.Time)
}

// Allow consumes one token from (bucket,key). When the bucket is empty,
// retryAfter is the wall-clock duration until it refills.
func (l *tokenBucketLimiter) Allow(_ context.Context, bucket, key string) (bool, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.nowFunc()
	id := bucket + "|" + key
	next, seen := l.nextOK[id]
	if !seen || !now.Before(next) {
		l.nextOK[id] = now.Add(l.cooldown)
		return true, 0, nil
	}
	retry := next.Sub(now)
	if retry < time.Second {
		retry = time.Second
	}
	return false, retry, nil
}
