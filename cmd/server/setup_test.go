package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
)

// fakePool is a no-op pgstore.PgxConn so buildStack can construct stores
// without a real Postgres connection. The reconciler/source use a stub
// source and the publisher always errors, so no SQL ever fires here.
type fakePool struct{}

func (fakePool) QueryRow(context.Context, string, ...any) pgx.Row {
	return errRow{err: errors.New("fake pool: not implemented")}
}

func (fakePool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("fake pool: not implemented")
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

func fakePoolOpener(_ context.Context, _ string) (pgstore.PgxConn, func(), error) {
	closed := false
	return fakePool{}, func() { closed = true; _ = closed }, nil
}

func TestBuildStack_FlagOff_StubHandlerReturns200(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	stack, err := buildStack(context.Background(), cfg, logger, nil)
	if err != nil {
		t.Fatalf("buildStack: %v", err)
	}
	defer stack.Close()
	if stack.enabled {
		t.Fatal("stack.enabled = true, want false (flag off)")
	}
	if stack.reconciler != nil {
		t.Fatal("reconciler should be nil with flag off")
	}

	srv := httptest.NewServer(buildMux(stack))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhooks/whatsapp/some-token", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	healthResp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", healthResp.StatusCode)
	}
}

func TestBuildStack_FlagOn_BuildsRealService(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.WebhookV2Enabled = true
	cfg.MetaAppSecret = "topsecret"
	cfg.DatabaseURL = "postgres://ignored"
	cfg.NATSValidateStream = true
	cfg.NATSStreamDuplicatesWindow = time.Hour

	stack, err := buildStack(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), fakePoolOpener)
	if err != nil {
		t.Fatalf("buildStack: %v", err)
	}
	defer stack.Close()

	if !stack.enabled {
		t.Fatal("stack.enabled = false, want true")
	}
	if stack.reconciler == nil {
		t.Fatal("reconciler is nil")
	}
	if stack.probe == nil {
		t.Fatal("probe is nil")
	}
	if stack.registry == nil {
		t.Fatal("registry is nil")
	}

	mux := buildMux(stack)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Smoke F23 — anti-enumeration: unknown channel still returns 200.
	resp, err := http.Post(srv.URL+"/webhooks/whatsapp/unknown-token", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// /metrics is exposed when the registry is wired.
	mresp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer mresp.Body.Close()
	if mresp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d", mresp.StatusCode)
	}

	// /health is reconciler-aware: before the first tick it should be 503.
	hresp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer hresp.Body.Close()
	if hresp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/health pre-tick status = %d, want 503", hresp.StatusCode)
	}

	// One reconciler tick records lastFetch via the probe wrapper.
	if err := stack.reconciler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if stack.probe.LastFetch().IsZero() {
		t.Fatal("probe.LastFetch is zero after a successful tick")
	}

	hresp2, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer hresp2.Body.Close()
	if hresp2.StatusCode != http.StatusOK {
		t.Fatalf("/health post-tick status = %d, want 200", hresp2.StatusCode)
	}
}

func TestBuildStack_FlagOn_FailsFastWhenDuplicatesWindowTooSmall(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.WebhookV2Enabled = true
	cfg.MetaAppSecret = "x"
	cfg.DatabaseURL = "postgres://x"
	cfg.NATSStreamDuplicatesWindow = 30 * time.Minute // < 1h, F-14 violation

	_, err := buildStack(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), fakePoolOpener)
	if err == nil {
		t.Fatal("expected fail-fast on Duplicates<1h")
	}
	if !strings.Contains(err.Error(), "Duplicates") {
		t.Fatalf("error message missing Duplicates context: %v", err)
	}
}

func TestBuildStack_FlagOn_SkipsValidationWhenDisabled(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.WebhookV2Enabled = true
	cfg.MetaAppSecret = "x"
	cfg.DatabaseURL = "postgres://x"
	cfg.NATSValidateStream = false
	cfg.NATSStreamDuplicatesWindow = time.Second // would fail if validated

	stack, err := buildStack(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), fakePoolOpener)
	if err != nil {
		t.Fatalf("buildStack: %v", err)
	}
	defer stack.Close()
}

func TestBuildStack_FlagOn_RejectsBadMetaChannel(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.WebhookV2Enabled = true
	cfg.MetaAppSecret = "x"
	cfg.DatabaseURL = "postgres://x"
	cfg.MetaChannels = []string{"telegram"} // unsupported by Meta adapter

	_, err := buildStack(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), fakePoolOpener)
	if err == nil {
		t.Fatal("expected error for unsupported channel")
	}
	if !strings.Contains(err.Error(), "telegram") {
		t.Fatalf("error message missing channel: %v", err)
	}
}

func TestBuildStack_FlagOn_PoolOpenerError(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.WebhookV2Enabled = true
	cfg.MetaAppSecret = "x"
	cfg.DatabaseURL = "postgres://x"

	openErr := errors.New("pool open failed")
	opener := func(context.Context, string) (pgstore.PgxConn, func(), error) {
		return nil, nil, openErr
	}
	_, err := buildStack(context.Background(), cfg, nil, opener)
	if !errors.Is(err, openErr) {
		t.Fatalf("err = %v, want openErr", err)
	}
}

func TestStack_CloseSafeOnNil(t *testing.T) {
	t.Parallel()
	var s *stack
	s.Close() // should not panic
	(&stack{}).Close()
}

func TestDefaultPoolOpener_BadURLReturnsError(t *testing.T) {
	t.Parallel()
	_, _, err := defaultPoolOpener(context.Background(), "not://a/valid/postgres/url")
	if err == nil {
		t.Fatal("expected error from defaultPoolOpener with bad URL")
	}
}
