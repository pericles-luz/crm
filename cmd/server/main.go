// Package main is the CRM HTTP server entrypoint (SIN-62208 Fase 0 PR1).
//
// Two HTTP listeners run concurrently when the SIN-62243 F45 stack is
// wired:
//
//   - Public listener (HTTP_ADDR, default :8080) — routes the public
//     surface (/health today; tenant routes incrementally).
//   - Internal listener (INTERNAL_HTTP_ADDR, default :8081) — exposes
//     ONLY /internal/tls/ask. Bound for docker-internal reachability;
//     compose does NOT publish this port to the host.
//
// The internal listener is wired only when DATABASE_URL and REDIS_URL are
// both present so cmd/server tests / smoke runs without those deps still
// boot the public listener cleanly. When skipped, /internal/tls/ask
// returns 404 from the public listener; this is the F45 acceptance
// criterion "endpoint não responde quando bateado em interface pública".
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	goredis "github.com/redis/go-redis/v9"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	crmslog "github.com/pericles-luz/crm/internal/adapter/observability/slog"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	tlsasktransport "github.com/pericles-luz/crm/internal/adapter/transport/http/tlsask"
	"github.com/pericles-luz/crm/internal/customdomain/featureflag"
	"github.com/pericles-luz/crm/internal/customdomain/ratelimit/sliding"
	"github.com/pericles-luz/crm/internal/customdomain/tls_ask"
)

const (
	defaultAddr         = ":8080"
	defaultInternalAddr = ":8081"

	envHTTPAddr     = "HTTP_ADDR"
	envInternalAddr = "INTERNAL_HTTP_ADDR"
	envRedisURL     = "REDIS_URL"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(executeAll(ctx, os.Getenv))
}

func execute(ctx context.Context, getenv func(string) string) int {
	addr := defaultAddr
	if v := getenv(envHTTPAddr); v != "" {
		addr = v
	}
	if err := run(ctx, addr); err != nil {
		log.Printf("crm: %v", err)
		return 1
	}
	return 0
}

// executeAll runs the public listener and, when wired, the internal
// /internal/tls/ask listener concurrently. It returns 0 on graceful
// shutdown of both, 1 if either errors.
func executeAll(ctx context.Context, getenv func(string) string) int {
	return executeAllWith(ctx, getenv, defaultDial)
}

func executeAllWith(ctx context.Context, getenv func(string) string, dial dialFn) int {
	publicAddr := defaultAddr
	if v := getenv(envHTTPAddr); v != "" {
		publicAddr = v
	}
	internalAddr := defaultInternalAddr
	if v := getenv(envInternalAddr); v != "" {
		internalAddr = v
	}

	internalHandler, internalCleanup := buildInternalHandlerWith(ctx, getenv, dial)
	defer internalCleanup()

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	collectErr := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := run(ctx, publicAddr); err != nil {
			collectErr(fmt.Errorf("public listener: %w", err))
		}
	}()
	if internalHandler != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runInternal(ctx, internalAddr, internalHandler); err != nil {
				collectErr(fmt.Errorf("internal listener: %w", err))
			}
		}()
	}
	wg.Wait()

	if firstErr != nil {
		log.Printf("crm: %v", firstErr)
		return 1
	}
	return 0
}

func run(ctx context.Context, addr string) error {
	mux := newMux()
	cdHandler, cdCleanup := buildCustomDomainHandler(ctx, os.Getenv)
	defer cdCleanup()
	if cdHandler != nil {
		// SIN-62259 routes are mounted at the root of the public mux. The
		// handler returned by buildCustomDomainHandler already includes the
		// /static/ tree.
		mux.Handle("/", cdHandler)
		log.Printf("crm: custom-domain UI mounted on public listener")
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("crm: public listener on %s", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// runInternal serves ONLY /internal/tls/ask. Any other path returns 404.
// Caddy reaches this listener via the docker network (compose service
// name "app" + INTERNAL_HTTP_ADDR's port); the host network never sees
// it because compose does not publish the port.
func runInternal(ctx context.Context, addr string, handler http.Handler) error {
	mux := http.NewServeMux()
	mux.Handle(tlsasktransport.Path, handler)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("crm: internal listener on %s", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	return mux
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// dependencies bundles the external clients buildInternalHandler needs.
// It exists so tests can substitute fakes without monkey-patching pgpool
// or goredis package globals.
type dependencies struct {
	pool poolCloser
	rdb  redisCloser
}

type poolCloser interface {
	pgstore.PgxConn
	Close()
}

type redisCloser interface {
	sliding.Cmdable
	Ping(ctx context.Context) *goredis.StatusCmd
	Close() error
}

// dialFn opens the external clients. Production wiring goes through
// pgpool.New + goredis.NewClient; tests inject a stub.
type dialFn func(ctx context.Context, getenv func(string) string) (*dependencies, error)

// defaultDial is the production dialFn.
func defaultDial(ctx context.Context, getenv func(string) string) (*dependencies, error) {
	dsn := getenv(pgpool.EnvDSN)
	redisURL := getenv(envRedisURL)
	pool, err := pgpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pg connect: %w", err)
	}
	opt, err := goredis.ParseURL(redisURL)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("redis url: %w", err)
	}
	rdb := goredis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		pool.Close()
		_ = rdb.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &dependencies{pool: pool, rdb: rdb}, nil
}

// buildInternalHandler wires the F45 tls_ask use-case against the
// running process's Postgres + Redis. Returns (nil, no-op) when either
// dep is not configured or unreachable so cmd/server stays bootable in
// environments without those services.
func buildInternalHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	return buildInternalHandlerWith(ctx, getenv, defaultDial)
}

func buildInternalHandlerWith(ctx context.Context, getenv func(string) string, dial dialFn) (http.Handler, func()) {
	noop := func() {}
	dsn := getenv(pgpool.EnvDSN)
	redisURL := getenv(envRedisURL)
	if dsn == "" || redisURL == "" {
		log.Printf("crm: internal listener disabled (DATABASE_URL/REDIS_URL unset)")
		return nil, noop
	}

	deps, err := dial(ctx, getenv)
	if err != nil {
		log.Printf("crm: internal listener disabled — %v", err)
		return nil, noop
	}

	repo := pgstore.NewTLSAskLookup(deps.pool)
	rate := sliding.New(deps.rdb, "customdomain:tls_ask", 3, time.Minute)
	flag := featureflag.NewFromEnv(getenv)
	logger := crmslog.NewTLSAskLogger(slog.Default())
	uc := tls_ask.New(repo, rate, flag, logger, time.Now)
	handler := tlsasktransport.New(uc)

	cleanup := func() {
		deps.pool.Close()
		_ = deps.rdb.Close()
	}
	return handler, cleanup
}
