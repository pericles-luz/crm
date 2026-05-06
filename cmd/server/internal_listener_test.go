package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	goredis "github.com/redis/go-redis/v9"

	tlsasktransport "github.com/pericles-luz/crm/internal/adapter/transport/http/tlsask"
)

var errBoom = errors.New("boom")

type fakePool struct {
	onClose func()
}

func (f *fakePool) QueryRow(context.Context, string, ...any) pgx.Row {
	return nil
}
func (f *fakePool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errBoom
}
func (f *fakePool) Close() {
	if f.onClose != nil {
		f.onClose()
	}
}

type fakeRedis struct{}

func (f *fakeRedis) Eval(context.Context, string, []string, ...any) *goredis.Cmd {
	return &goredis.Cmd{}
}
func (f *fakeRedis) Ping(context.Context) *goredis.StatusCmd { return &goredis.StatusCmd{} }
func (f *fakeRedis) Close() error                            { return nil }

// TestPublicListenerDoesNotExposeInternalRoute is the F45 acceptance
// criterion: "Endpoint /internal/tls/ask não responde quando bateado em
// interface pública (integration test)." The public mux must return 404
// for the path that the internal listener owns.
func TestPublicListenerDoesNotExposeInternalRoute(t *testing.T) {
	t.Parallel()
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, addr) }()
	t.Cleanup(func() {
		cancel()
		<-errCh
	})
	waitForListening(t, addr)

	for _, path := range []string{
		tlsasktransport.Path,
		tlsasktransport.Path + "?domain=shop.example.com",
		"/internal/tls/ask/",
	} {
		path := path
		t.Run(strings.NewReplacer("/", "_", "?", "-").Replace(path), func(t *testing.T) {
			res, err := http.Get("http://" + addr + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusNotFound {
				t.Fatalf("public listener returned status %d for %q; want 404", res.StatusCode, path)
			}
		})
	}
}

// TestInternalListenerServesAskRoute proves the internal listener does
// expose /internal/tls/ask and refuses unknown paths with 404. This
// pairs with the test above to prove the route only exists on the
// internal listener.
func TestInternalListenerServesAskRoute(t *testing.T) {
	t.Parallel()
	addr := freePort(t)
	// A canned handler so the test does not require Postgres/Redis.
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- runInternal(ctx, addr, handler) }()
	t.Cleanup(func() {
		cancel()
		<-errCh
	})
	waitForListening(t, addr)

	res, err := http.Get("http://" + addr + tlsasktransport.Path)
	if err != nil {
		t.Fatalf("GET internal: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusTeapot {
		t.Fatalf("internal listener returned %d for %s; want 418 (teapot via canned handler)", res.StatusCode, tlsasktransport.Path)
	}

	// Sanity: any other path on the internal listener returns 404 — the
	// internal listener exposes ONLY /internal/tls/ask, never /health.
	res2, err := http.Get("http://" + addr + "/health")
	if err != nil {
		t.Fatalf("GET internal /health: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusNotFound {
		t.Fatalf("internal /health returned %d; want 404", res2.StatusCode)
	}
}

// TestRunInternal_ShutsDownOnContextCancel mirrors the existing run()
// shutdown test for the internal variant.
func TestRunInternal_ShutsDownOnContextCancel(t *testing.T) {
	t.Parallel()
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- runInternal(ctx, addr, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	}()
	waitForListening(t, addr)
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runInternal returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runInternal did not return after cancel")
	}
}

// TestRunInternal_InvalidAddrReturnsError covers the error path.
func TestRunInternal_InvalidAddrReturnsError(t *testing.T) {
	t.Parallel()
	if err := runInternal(context.Background(), "not-a-valid-addr", http.NewServeMux()); err == nil {
		t.Fatal("runInternal with invalid addr returned nil, want error")
	}
}

// TestExecuteAll_BringsUpPublicWithoutInternalDeps proves executeAll
// boots the public listener cleanly when DATABASE_URL / REDIS_URL are
// unset (no internal listener) — the SIN-62208 baseline contract.
func TestExecuteAll_BringsUpPublicWithoutInternalDeps(t *testing.T) {
	t.Parallel()
	publicAddr := freePort(t)
	getenv := func(k string) string {
		if k == envHTTPAddr {
			return publicAddr
		}
		return ""
	}
	ctx, cancel := context.WithCancel(context.Background())
	codeCh := make(chan int, 1)
	go func() { codeCh <- executeAll(ctx, getenv) }()
	waitForListening(t, publicAddr)
	cancel()
	select {
	case code := <-codeCh:
		if code != 0 {
			t.Fatalf("executeAll returned %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("executeAll did not return after cancel")
	}
}

// TestExecuteAllWith_BringsUpBothListeners_StubDial proves executeAll
// brings up both the public and the internal listener concurrently when
// the dial succeeds, and that both cleanly stop on context cancel.
func TestExecuteAllWith_BringsUpBothListeners_StubDial(t *testing.T) {
	t.Parallel()
	publicAddr := freePort(t)
	internalAddr := freePort(t)
	getenv := func(k string) string {
		switch k {
		case envHTTPAddr:
			return publicAddr
		case envInternalAddr:
			return internalAddr
		case "DATABASE_URL":
			return "postgres://x"
		case envRedisURL:
			return "redis://x"
		}
		return ""
	}
	dial := func(context.Context, func(string) string) (*dependencies, error) {
		return &dependencies{pool: &fakePool{}, rdb: &fakeRedis{}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	codeCh := make(chan int, 1)
	go func() { codeCh <- executeAllWith(ctx, getenv, dial) }()
	waitForListening(t, publicAddr)
	waitForListening(t, internalAddr)

	cancel()
	select {
	case code := <-codeCh:
		if code != 0 {
			t.Fatalf("executeAllWith returned %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("executeAllWith did not return after cancel")
	}
}

// TestExecuteAllWith_PropagatesPublicListenerError proves executeAll
// returns 1 when the public listener errors (e.g. invalid addr).
func TestExecuteAllWith_PropagatesPublicListenerError(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == envHTTPAddr {
			return "definitely-invalid-addr"
		}
		return ""
	}
	dial := func(context.Context, func(string) string) (*dependencies, error) {
		return nil, errBoom
	}
	if code := executeAllWith(context.Background(), getenv, dial); code != 1 {
		t.Fatalf("executeAllWith returned %d, want 1", code)
	}
}

// TestBuildInternalHandler_DisabledWhenDepsUnset proves the internal
// listener stays nil when DATABASE_URL or REDIS_URL is missing.
func TestBuildInternalHandler_DisabledWhenDepsUnset(t *testing.T) {
	t.Parallel()
	cases := map[string]func(string) string{
		"both_unset": func(string) string { return "" },
		"only_db": func(k string) string {
			if k == "DATABASE_URL" {
				return "postgres://x"
			}
			return ""
		},
		"only_redis": func(k string) string {
			if k == envRedisURL {
				return "redis://x"
			}
			return ""
		},
	}
	for name, getenv := range cases {
		getenv := getenv
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, cleanup := buildInternalHandler(context.Background(), getenv)
			t.Cleanup(cleanup)
			if h != nil {
				t.Fatalf("expected nil handler when deps unset")
			}
		})
	}
}

func TestBuildInternalHandler_DialErrorReturnsNil(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case "DATABASE_URL":
			return "postgres://x"
		case envRedisURL:
			return "redis://x"
		}
		return ""
	}
	dial := func(context.Context, func(string) string) (*dependencies, error) {
		return nil, errBoom
	}
	h, cleanup := buildInternalHandlerWith(context.Background(), getenv, dial)
	t.Cleanup(cleanup)
	if h != nil {
		t.Fatal("expected nil handler when dial errors")
	}
}

func TestBuildInternalHandler_DialSuccessReturnsHandler(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case "DATABASE_URL":
			return "postgres://x"
		case envRedisURL:
			return "redis://x"
		}
		return ""
	}
	closed := false
	dial := func(context.Context, func(string) string) (*dependencies, error) {
		return &dependencies{
			pool: &fakePool{onClose: func() { closed = true }},
			rdb:  &fakeRedis{},
		}, nil
	}
	h, cleanup := buildInternalHandlerWith(context.Background(), getenv, dial)
	if h == nil {
		t.Fatal("expected non-nil handler when dial succeeds")
	}
	cleanup()
	if !closed {
		t.Fatal("cleanup did not close pool")
	}
}

// _ keeps go vet happy if listing changes.
var _ = net.Listen
