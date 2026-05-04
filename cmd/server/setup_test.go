package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	"github.com/pericles-luz/crm/internal/webhook"
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

// fakePublisher implements webhook.EventPublisher with a counted no-op
// so tests can verify reconciler/handler wire-up without dialing NATS.
type fakePublisher struct {
	calls atomic.Int64
}

func (p *fakePublisher) Publish(_ context.Context, _ [16]byte, _ webhook.TenantID, _ string, _ []byte, _ map[string][]string) error {
	p.calls.Add(1)
	return nil
}

func fakePublisherFactory(_ context.Context, _ config, _ *slog.Logger) (webhook.EventPublisher, func(), error) {
	closed := false
	return &fakePublisher{}, func() { closed = true; _ = closed }, nil
}

func TestBuildStack_FlagOff_StubHandlerReturns200(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	stack, err := buildStack(context.Background(), cfg, logger, nil, nil)
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

	stack, err := buildStack(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), fakePoolOpener, fakePublisherFactory)
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

func TestBuildStack_FlagOn_PublisherFactoryError(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.WebhookV2Enabled = true
	cfg.MetaAppSecret = "x"
	cfg.DatabaseURL = "postgres://x"

	pubErr := errors.New("nats publisher failed")
	failingPublisher := func(context.Context, config, *slog.Logger) (webhook.EventPublisher, func(), error) {
		return nil, nil, pubErr
	}
	_, err := buildStack(context.Background(), cfg, nil, fakePoolOpener, failingPublisher)
	if !errors.Is(err, pubErr) {
		t.Fatalf("err = %v, want pubErr", err)
	}
}

func TestBuildStack_FlagOn_RejectsBadMetaChannel(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.WebhookV2Enabled = true
	cfg.MetaAppSecret = "x"
	cfg.DatabaseURL = "postgres://x"
	cfg.MetaChannels = []string{"telegram"} // unsupported by Meta adapter

	_, err := buildStack(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), fakePoolOpener, fakePublisherFactory)
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
	_, err := buildStack(context.Background(), cfg, nil, opener, fakePublisherFactory)
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

func TestDefaultPublisherFactory_RequiresNATSURL(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.NATSURL = ""
	_, _, err := defaultPublisherFactory(context.Background(), cfg, nil)
	if err == nil {
		t.Fatal("expected error when NATS URL is empty")
	}
}

func TestDefaultPublisherFactory_BadTLSCAFileReturnsError(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.NATSURL = "nats://localhost:4222"
	cfg.NATSTLSCAFile = "/nonexistent/ca.pem"
	_, _, err := defaultPublisherFactory(context.Background(), cfg, nil)
	if err == nil {
		t.Fatal("expected error from missing CA file")
	}
	if !strings.Contains(err.Error(), "tls") {
		t.Fatalf("error should mention tls: %v", err)
	}
}

func TestDefaultPublisherFactory_DialErrorSurfaces(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	// Reserved TEST-NET-1 with a closed port; dial should fail fast.
	cfg.NATSURL = "nats://192.0.2.1:4222"
	cfg.NATSReconnectWait = 10 * time.Millisecond
	cfg.NATSMaxReconnects = 0
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, _, err := defaultPublisherFactory(ctx, cfg, nil)
	if err == nil {
		t.Fatal("expected dial error to surface")
	}
}

func TestChainClosers_RunsInOrderAndSkipsNil(t *testing.T) {
	t.Parallel()
	if got := chainClosers(); got != nil {
		t.Fatal("chainClosers() = non-nil for empty input")
	}
	var order []int
	c := chainClosers(func() { order = append(order, 1) }, nil, func() { order = append(order, 2) })
	c()
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("order = %v", order)
	}
}

func TestLoadNATSTLSConfig_NilWhenAllEmpty(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	tlsCfg, err := loadNATSTLSConfig(cfg)
	if err != nil {
		t.Fatalf("loadNATSTLSConfig: %v", err)
	}
	if tlsCfg != nil {
		t.Fatalf("tlsCfg = %+v, want nil", tlsCfg)
	}
}

func TestLoadNATSTLSConfig_ServerNameOnly(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.NATSTLSServerName = "nats.example.com"
	tlsCfg, err := loadNATSTLSConfig(cfg)
	if err != nil {
		t.Fatalf("loadNATSTLSConfig: %v", err)
	}
	if tlsCfg == nil || tlsCfg.ServerName != "nats.example.com" {
		t.Fatalf("tlsCfg = %+v", tlsCfg)
	}
}

func TestLoadNATSTLSConfig_BadCAFile(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.NATSTLSCAFile = "/no/such/path"
	if _, err := loadNATSTLSConfig(cfg); err == nil {
		t.Fatal("expected error from missing CA")
	}
}
