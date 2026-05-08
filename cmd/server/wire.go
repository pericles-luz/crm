package main

// SIN-62348 wire-up: builds the dependency graph the production HTTP
// server needs to serve POST /login with the SIN-62341 lockout +
// rate-limit chain. cmd/server is excluded from the project's >85%
// coverage rule (assemble code is exercised end-to-end by smoke
// staging, not unit-level), so this file optimises for clarity over
// fine-grained testability.
//
// The wire is a strict layered build:
//
//   1. Postgres pool (DATABASE_URL).
//   2. Redis client (REDIS_URL) → SlidingWindow rate limiter.
//   3. Slack notifier (SLACK_WEBHOOK_URL — empty ⇒ no-op).
//   4. tenancy.Resolver, iam.UserCredentialReader, iam.SessionStore.
//   5. The default policy table (DefaultPolicies()).
//
// HTTP composition (newAppMux):
//
//   - /health, /login (GET+POST), /logout, /hello-tenant are mounted
//     via httpapi.NewRouter (the chi router from SIN-62217). The
//     middleware chain stitched there is:
//
//         RequestID → RealIP → Logger → Recoverer → TenantScope → Auth
//
//     /health bypasses TenantScope (LB liveness must work without
//     tenant resolution); /hello-tenant requires Auth (cookie session
//     validated against iam.ValidateSession). /login GET+POST and
//     /logout sit under TenantScope but outside Auth.
//   - tenantIAMAdapter is the seam between the chi handlers and the
//     SIN-62341 lockout chain: Login is per-request (NewTenantLockouts
//     captures tenant.ID), Logout/ValidateSession use a global Service
//     literal because they don't touch the lockout port.
//
// The master Service factory is built but the master HTTP routes
// (POST /m/login etc.) are deferred to the master-MFA ticket
// (ADR 0074 / SIN-62338): wiring them now is premature without the
// master session/cookie surface they depend on. The factory is
// exported via newMasterServiceFactory so that ticket can plug it in
// without re-doing the lockout/limiter/alerter assembly.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/loginhandler"
	"github.com/pericles-luz/crm/internal/adapter/notify/slack"
	rlredis "github.com/pericles-luz/crm/internal/adapter/ratelimit/redis"
	"github.com/pericles-luz/crm/internal/iam"
	domainratelimit "github.com/pericles-luz/crm/internal/iam/ratelimit"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// envRedisURL is the canonical key for the Redis DSN. Mirrors
// postgres.EnvDSN style so docs/operations/env.md has one shape.
const envRedisURL = "REDIS_URL"

// envSlackWebhook is the optional Slack webhook for the master
// lockout alerter. Empty ⇒ no-op (slack.New documents the contract).
const envSlackWebhook = "SLACK_WEBHOOK_URL"

// envCookieSecure overrides the default-true Secure attribute on the
// session cookie. Read by cookieSecureFromEnv; only the literal
// strings "false", "0", and "off" (any case, trimmed) flip it off.
const envCookieSecure = "COOKIE_SECURE"

// cookieSecureFromEnv returns whether the session cookie's Secure
// attribute should be set. Defaults to true so production deployments
// (which never set the env var) get the safe behaviour by default;
// only an explicit opt-out flips it off.
func cookieSecureFromEnv(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(envCookieSecure))) {
	case "false", "0", "off":
		return false
	default:
		return true
	}
}

// deps is the assembled dependency graph the HTTP layer consumes.
// Closed via cleanup returned from assembleDeps.
type deps struct {
	pool          *pgxpool.Pool
	redis         *goredis.Client
	limiter       domainratelimit.RateLimiter
	policies      map[string]domainratelimit.Policy
	notifier      *slack.Notifier
	tenants       tenancy.Resolver
	users         *postgresadapter.UserCredentialReader
	sessions      *postgresadapter.SessionStore
	logger        *slog.Logger
	masterService masterServiceFactory
	// cookieSecure flips the Secure attribute on the session cookie
	// the chi login handler writes. Defaults to true (production-safe);
	// COOKIE_SECURE=false unsets it for plaintext local-dev / test
	// servers (httptest.Server is plain HTTP, so cookies with Secure
	// would be dropped by the test client).
	cookieSecure bool
}

// assembleDeps wires the production dependency graph from environment
// configuration. Returns a cleanup function the caller MUST defer to
// release pool + Redis on shutdown.
func assembleDeps(ctx context.Context, getenv func(string) string, logger *slog.Logger) (*deps, func(), error) {
	if logger == nil {
		logger = slog.Default()
	}
	pool, err := postgresadapter.NewFromEnv(ctx, getenv)
	if err != nil {
		return nil, nil, fmt.Errorf("cmd/server: postgres pool: %w", err)
	}

	rdb, err := openRedis(ctx, getenv(envRedisURL))
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("cmd/server: redis client: %w", err)
	}

	policies, err := domainratelimit.DefaultPolicies()
	if err != nil {
		pool.Close()
		_ = rdb.Close()
		return nil, nil, fmt.Errorf("cmd/server: default policies: %w", err)
	}
	limiter := rlredis.New(rdb, "auth:rl:")

	notifier := slack.New(getenv(envSlackWebhook))

	tenantsRes, err := postgresadapter.NewTenantResolver(pool)
	if err != nil {
		pool.Close()
		_ = rdb.Close()
		return nil, nil, fmt.Errorf("cmd/server: tenant resolver: %w", err)
	}

	users := postgresadapter.NewUserCredentialReader(pool)
	sessions := postgresadapter.NewSessionStore(pool)

	masterFactory := newMasterServiceFactory(masterFactoryDeps{
		pool:     pool,
		tenants:  tenantsRes,
		users:    users,
		sessions: sessions,
		limiter:  limiter,
		policy:   policies["m_login"],
		alerter:  notifier,
		logger:   logger,
	})

	d := &deps{
		pool:          pool,
		redis:         rdb,
		limiter:       limiter,
		policies:      policies,
		notifier:      notifier,
		tenants:       tenantsRes,
		users:         users,
		sessions:      sessions,
		logger:        logger,
		masterService: masterFactory,
		cookieSecure:  cookieSecureFromEnv(getenv),
	}
	cleanup := func() {
		d.pool.Close()
		_ = d.redis.Close()
	}
	return d, cleanup, nil
}

// openRedis parses the URL, opens a client, and pings to fail fast on
// a misconfigured DSN. An empty URL is rejected — the rate-limit
// path REQUIRES Redis to function (the per-bucket pre-check is the
// only place that needs it).
func openRedis(ctx context.Context, url string) (*goredis.Client, error) {
	if url == "" {
		return nil, errors.New("REDIS_URL is empty")
	}
	opts, err := goredis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	client := goredis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return client, nil
}

// newAppMux returns the production HTTP handler. Originally a stdlib
// mux, the implementation now delegates to httpapi.NewRouter (the chi
// router from SIN-62217); the symbol name is preserved so the
// existing wire_test.go coverage (TestNewAppMux_*) continues to
// describe the wireup boundary. The route shape /health, /login is
// unchanged — only the multiplexer underneath changed, plus the new
// /logout and /hello-tenant routes from SIN-62217 §Routes.
func newAppMux(d *deps) http.Handler {
	return httpapi.NewRouter(httpapi.Deps{
		IAM:            tenantIAMAdapter{deps: d},
		TenantResolver: d.tenants,
		Logger:         d.logger,
		CookieSecure:   d.cookieSecure,
	})
}

// tenantIAMAdapter satisfies httpapi.IAMService. It bridges PR #7's
// chi router to the SIN-62341 lockout chain.
//
// Login routes through tenantLoginAdapter so each request gets a
// tenant-scoped *iam.Service with a NewTenantLockouts(pool, tenant.ID)
// adapter wired in — that is the only iam.Service method that touches
// the lockout port, so per-request allocation is necessary.
//
// Logout and ValidateSession build a fresh, lockout-free
// *iam.Service literal: both methods take tenantID as an explicit
// argument and never call into iam/ratelimit.Lockouts, so they share
// a global wiring without any per-tenant construction cost.
type tenantIAMAdapter struct {
	deps *deps
}

// Login forwards to tenantLoginAdapter so the per-request lockout
// adapter is wired in. The closure capture pattern means each call
// re-resolves the tenant from context (the chi router runs
// TenantScope first, so it is always populated when we reach here).
func (a tenantIAMAdapter) Login(ctx context.Context, host, email, password string, ip net.IP, ua string) (iam.Session, error) {
	return tenantLoginAdapter(a.deps)(ctx, host, email, password, ip, ua)
}

// Logout / ValidateSession do not need Lockouts — tenantID flows in
// as an explicit argument. We construct a minimal Service per call
// (the alternative is caching one in deps; the cost is identical
// because *iam.Service is a struct of interface pointers).
func (a tenantIAMAdapter) Logout(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	return a.lockoutFreeService().Logout(ctx, tenantID, sessionID)
}

func (a tenantIAMAdapter) ValidateSession(ctx context.Context, tenantID, sessionID uuid.UUID) (iam.Session, error) {
	return a.lockoutFreeService().ValidateSession(ctx, tenantID, sessionID)
}

func (a tenantIAMAdapter) lockoutFreeService() *iam.Service {
	return &iam.Service{
		Tenants:  iamTenantResolver{inner: a.deps.tenants},
		Users:    a.deps.users,
		Sessions: a.deps.sessions,
		Logger:   a.deps.logger,
	}
}

// tenantLoginAdapter returns the LoginFunc the loginhandler consumes.
// For each request it pulls the resolved tenant from context, builds
// a TenantLockouts adapter scoped to that tenant, and assembles a
// fresh iam.Service. The Service struct is small (interface pointers
// + config), so per-request allocation is cheap; the alternative —
// one global Service — does not work because iam/ratelimit.Lockouts
// is a tenant-scoped port (NewTenantLockouts captures tenantID at
// construction).
func tenantLoginAdapter(d *deps) loginhandler.LoginFunc {
	return func(ctx context.Context, host, email, password string, ip net.IP, ua string) (iam.Session, error) {
		tenant, err := tenancy.FromContext(ctx)
		if err != nil {
			return iam.Session{}, fmt.Errorf("cmd/server: %w", err)
		}
		lockouts, err := postgresadapter.NewTenantLockouts(d.pool, tenant.ID)
		if err != nil {
			return iam.Session{}, fmt.Errorf("cmd/server: tenant lockouts: %w", err)
		}
		svc := &iam.Service{
			Tenants:     iamTenantResolver{inner: d.tenants},
			Users:       d.users,
			Sessions:    d.sessions,
			Logger:      d.logger,
			Lockouts:    lockouts,
			Limiter:     d.limiter,
			LoginPolicy: d.policies["login"],
			// Alerter intentionally nil for tenant — only master
			// endpoints fire the synchronous Slack alert (ADR 0073 §D4).
		}
		return svc.Login(ctx, host, email, password, ip, ua)
	}
}

// iamTenantResolver bridges tenancy.Resolver (returns *tenancy.Tenant)
// to iam.TenantResolver (returns uuid.UUID + iam-flavoured sentinel).
// Defining this in cmd/server keeps the iam package free of any
// tenancy import — iam stays pure-domain.
type iamTenantResolver struct {
	inner tenancy.Resolver
}

func (r iamTenantResolver) ResolveByHost(ctx context.Context, host string) (uuid.UUID, error) {
	t, err := r.inner.ResolveByHost(ctx, host)
	if err != nil {
		if errors.Is(err, tenancy.ErrTenantNotFound) {
			return uuid.Nil, iam.ErrTenantNotFound
		}
		return uuid.Nil, err
	}
	return t.ID, nil
}

// masterFactoryDeps groups the shared inputs the master Service
// factory needs. The factory itself yields a fresh *iam.Service per
// master request, with MasterLockouts(pool, actorID) scoped to the
// authenticated master operator (the actorID flows from the
// not-yet-implemented master session — see ADR 0074).
type masterFactoryDeps struct {
	pool     *pgxpool.Pool
	tenants  tenancy.Resolver
	users    *postgresadapter.UserCredentialReader
	sessions *postgresadapter.SessionStore
	limiter  domainratelimit.RateLimiter
	policy   domainratelimit.Policy
	alerter  *slack.Notifier
	logger   *slog.Logger
}

// masterServiceFactory builds an iam.Service scoped to a master
// operator. actorID is the authenticated master user_id from the
// request session; passing uuid.Nil is rejected by NewMasterLockouts
// upstream so a bypass is impossible.
//
// Per-request allocation is fine for the rare master-console traffic
// (a handful of operators, not customer-facing).
type masterServiceFactory func(actorID uuid.UUID) (*iam.Service, error)

func newMasterServiceFactory(d masterFactoryDeps) masterServiceFactory {
	return func(actorID uuid.UUID) (*iam.Service, error) {
		lockouts, err := postgresadapter.NewMasterLockouts(d.pool, actorID)
		if err != nil {
			return nil, fmt.Errorf("cmd/server: master lockouts: %w", err)
		}
		return &iam.Service{
			Tenants:     iamTenantResolver{inner: d.tenants},
			Users:       d.users,
			Sessions:    d.sessions,
			Logger:      d.logger,
			Lockouts:    lockouts,
			Limiter:     d.limiter,
			LoginPolicy: d.policy,
			Alerter:     d.alerter,
		}, nil
	}
}

// runApp is the production-mode entrypoint: assemble deps, mount the
// full router, listen, shut down on context cancel. Health-only mode
// (no DATABASE_URL set) keeps run() so the existing standalone tests
// + local-dev use case continue to work.
func runApp(ctx context.Context, addr string, getenv func(string) string, logger *slog.Logger) error {
	d, cleanup, err := assembleDeps(ctx, getenv, logger)
	if err != nil {
		return err
	}
	defer cleanup()

	srv := &http.Server{
		Addr:              addr,
		Handler:           newAppMux(d),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	logger.Info("crm: listening (app mode)", slog.String("addr", addr))
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
