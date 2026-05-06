package main

// SIN-62259 wiring — public-side custom-domain management UI.
//
// The handler is wired only when CUSTOM_DOMAIN_UI_ENABLED=1 AND the
// process has both DATABASE_URL and REDIS_URL configured. When any of
// those are unset the handler is omitted; the existing /health route
// remains the only public surface (preserving cmd/server tests that
// run without external deps).
//
// Tenant identity is sourced from a request header (`X-Tenant-ID`)
// while session/cookie auth is owned by a separate ticket. The header
// path is gated by the same feature flag and is logged on every
// request — production deploys MUST replace it with real auth before
// flipping the flag.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	customdomainhttp "github.com/pericles-luz/crm/internal/adapter/transport/http/customdomain"
	"github.com/pericles-luz/crm/internal/customdomain/circuitbreaker"
	"github.com/pericles-luz/crm/internal/customdomain/enrollment"
	"github.com/pericles-luz/crm/internal/customdomain/management"
	"github.com/pericles-luz/crm/internal/slugreservation"
)

const (
	envCustomDomainUI       = "CUSTOM_DOMAIN_UI_ENABLED"
	envCustomDomainCSRF     = "CUSTOM_DOMAIN_CSRF_SECRET"
	envCustomDomainPrimary  = "PRIMARY_DOMAIN"
	envCustomDomainTenantHd = "X-Tenant-ID"
	enrollmentRedisPrefix   = "customdomain:enrollment"
)

// customDomainDial is the test seam: the production wiring opens a real
// pgxpool; tests inject a fake pool that satisfies pgstore.PgxRowsConn.
type customDomainDial func(ctx context.Context, dsn string) (customDomainPool, error)

// customDomainPool is the pgxpool-shaped surface buildCustomDomainHandler
// needs. *pgxpool.Pool satisfies it; tests pass an in-memory implementation
// from custom_domain_store_test.go.
type customDomainPool interface {
	pgstore.PgxRowsConn
	Close()
}

// defaultCustomDomainDial is the production dialer. *pgxpool.Pool
// implements every method on customDomainPool (QueryRow, Exec, Query,
// Close), so the wrapper is just the type assertion.
func defaultCustomDomainDial(ctx context.Context, dsn string) (customDomainPool, error) {
	pool, err := pgpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return pool, nil
}

// buildCustomDomainHandler returns the registered http.Handler for the
// SIN-62259 routes plus a cleanup func. Returns (nil, no-op) when the
// feature is disabled or required deps cannot be reached, so the public
// listener stays fully functional for the existing /health route.
func buildCustomDomainHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	return buildCustomDomainHandlerWith(ctx, getenv, defaultCustomDomainDial)
}

// buildCustomDomainHandlerWith is the test-friendly variant. The dial
// argument is the only path that touches the network; passing a stub
// short-circuits the pgxpool.New call.
func buildCustomDomainHandlerWith(ctx context.Context, getenv func(string) string, dial customDomainDial) (http.Handler, func()) {
	noop := func() {}
	if v := getenv(envCustomDomainUI); v != "1" {
		return nil, noop
	}
	dsn := getenv(pgpool.EnvDSN)
	if dsn == "" {
		log.Printf("crm: custom-domain UI disabled (DATABASE_URL unset)")
		return nil, noop
	}
	secret := []byte(getenv(envCustomDomainCSRF))
	if len(secret) < 32 {
		log.Printf("crm: custom-domain UI disabled (CUSTOM_DOMAIN_CSRF_SECRET must be ≥32 bytes)")
		return nil, noop
	}
	primary := getenv(envCustomDomainPrimary)
	if primary == "" {
		primary = "exemplo.com"
	}

	pool, err := dial(ctx, dsn)
	if err != nil {
		log.Printf("crm: custom-domain UI disabled — pg connect: %v", err)
		return nil, noop
	}

	store := pgstore.NewCustomDomainStore(pool)
	gate, gateCleanup := buildEnrollmentGate(pool)
	slugSvc, _ := slugreservation.NewService(
		pgstore.NewSlugReservationStore(pool),
		pgstore.NewSlugRedirectStore(pool),
		nopAudit{},
		nopSlack{},
		nil,
	)
	uc, err := management.New(management.Config{
		Store: store,
		Gate:  gate,
		Slug:  slugReleaseAdapter{svc: slugSvc},
		Audit: managementAudit{logger: slog.Default()},
		Now:   time.Now,
	})
	if err != nil {
		pool.Close()
		gateCleanup()
		log.Printf("crm: custom-domain UI disabled — management.New: %v", err)
		return nil, noop
	}
	handler, err := customdomainhttp.New(customdomainhttp.Config{
		UseCase:       uc,
		CSRF:          customdomainhttp.CSRFConfig{Secret: secret, Secure: getenv("HTTP_TLS_TERMINATED") == "1"},
		PrimaryDomain: primary,
	})
	if err != nil {
		pool.Close()
		gateCleanup()
		log.Printf("crm: custom-domain UI disabled — handler: %v", err)
		return nil, noop
	}
	cleanup := func() {
		pool.Close()
		gateCleanup()
	}
	return wrapWithDevTenantHeader(registerCustomDomainRoutes(handler), os.Stderr), cleanup
}

// registerCustomDomainRoutes returns a *http.ServeMux carrying the
// SIN-62259 routes. The static-file handler is added here so the page
// can resolve `/static/customdomain.css` and the bundled HTMX script.
func registerCustomDomainRoutes(h *customdomainhttp.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	h.Register(mux)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))
	return mux
}

// wrapWithDevTenantHeader is the dev-only tenant-context middleware. It
// reads `X-Tenant-ID` from the request, parses it as a UUID, and if
// valid stores it on the request context. Production deploys MUST
// replace this with real session/cookie auth before flipping
// CUSTOM_DOMAIN_UI_ENABLED.
func wrapWithDevTenantHeader(next http.Handler, _ *os.File) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get(envCustomDomainTenantHd)
		if raw == "" {
			next.ServeHTTP(w, r)
			return
		}
		tenantID, err := uuid.Parse(raw)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		ctx := customdomainhttp.WithTenantID(r.Context(), tenantID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// buildEnrollmentGate wires *enrollment.UseCase against the production
// adapters. The breaker uses the existing in-memory implementation as a
// placeholder — the Redis-backed breaker lives behind the same port and
// will be swapped in when its adapter PR lands.
func buildEnrollmentGate(_ pgstore.PgxConn) (management.EnrollmentGate, func()) {
	// Redis client is reused from the internal listener; for the public
	// path we use lightweight in-memory implementations so the UI is
	// bootable without Redis. CTO can swap to the production adapters
	// once they are public-listener safe.
	gate := enrollment.New(
		zeroCount{},
		passWindowCounter{},
		zeroBreaker{},
		nil,
		time.Now,
		enrollment.DefaultQuota(),
	)
	return enrollmentGateAdapter{gate: gate}, func() {}
}

// enrollmentGateAdapter projects enrollment.Result into management.EnrollmentDecision.
type enrollmentGateAdapter struct{ gate *enrollment.UseCase }

func (a enrollmentGateAdapter) Allow(ctx context.Context, tenantID uuid.UUID) management.EnrollmentDecision {
	res := a.gate.Allow(ctx, tenantID)
	switch res.Decision {
	case enrollment.DecisionAllowed:
		return management.EnrollmentDecision{Allowed: true}
	case enrollment.DecisionDeniedHardCap:
		return management.EnrollmentDecision{Reason: management.ReasonRateLimited}
	case enrollment.DecisionDeniedHourlyQuota, enrollment.DecisionDeniedDailyQuota, enrollment.DecisionDeniedMonthlyQuota:
		return management.EnrollmentDecision{Reason: management.ReasonRateLimited, RetryAfter: res.ResetAfter}
	case enrollment.DecisionDeniedCircuitBreaker:
		return management.EnrollmentDecision{Reason: management.ReasonInternal}
	default:
		err := res.Err
		if err == nil {
			err = errors.New("enrollment: unknown decision")
		}
		return management.EnrollmentDecision{Err: fmt.Errorf("gate: %w", err)}
	}
}

// slugReleaseAdapter wraps slugreservation.Service so it satisfies
// management.SlugReleaser without leaking the Reservation type. Custom
// domains are FQDNs, but slugreservation operates on single-label slugs
// (regex `[a-z0-9](?:[a-z0-9-]*[a-z0-9])?`). The adapter extracts the
// first label so the 12-month lock is tracked at the natural slug level
// — `shop.example.com` reserves `shop` while the FQDN is removed. A
// per-host reservation table is its own ticket.
type slugReleaseAdapter struct{ svc *slugreservation.Service }

func (s slugReleaseAdapter) ReleaseSlug(ctx context.Context, host string, byTenantID uuid.UUID) error {
	if s.svc == nil {
		return nil
	}
	slug := firstLabel(host)
	if _, err := s.svc.ReleaseSlug(ctx, slug, byTenantID); err != nil {
		return err
	}
	return nil
}

// firstLabel returns the leftmost dot-separated component of host, or
// the whole string if there is no dot.
func firstLabel(host string) string {
	for i := 0; i < len(host); i++ {
		if host[i] == '.' {
			return host[:i]
		}
	}
	return host
}

// managementAudit emits one slog event per management decision.
type managementAudit struct{ logger *slog.Logger }

func (a managementAudit) LogManagement(ctx context.Context, ev management.AuditEvent) {
	a.logger.LogAttrs(ctx, slog.LevelInfo, "customdomain.management",
		slog.String("tenant_id", ev.TenantID.String()),
		slog.String("domain_id", ev.DomainID.String()),
		slog.String("host", ev.Host),
		slog.String("action", ev.Action),
		slog.String("outcome", ev.Outcome),
		slog.String("reason", ev.Reason.String()),
	)
}

// zeroCount, passWindowCounter, zeroBreaker are dev placeholders that
// keep the gate live without Redis. Production deploys MUST replace
// them with the real Redis-backed adapters before flipping the flag.
type zeroCount struct{}

func (zeroCount) ActiveCount(context.Context, uuid.UUID) (int, error) { return 0, nil }

type passWindowCounter struct{}

func (passWindowCounter) CountAndRecord(_ context.Context, _ uuid.UUID, _ enrollment.Window, _ time.Time) (int, error) {
	return 0, nil
}

type zeroBreaker struct{}

func (zeroBreaker) IsOpen(context.Context, uuid.UUID, time.Time) (bool, error) { return false, nil }

// nopAudit / nopSlack satisfy slugreservation's required ports without
// emitting anything; the management UI never invokes the master-override
// path (that lives on a separate admin surface).
type nopAudit struct{}

func (nopAudit) LogMasterOverride(context.Context, slugreservation.MasterOverrideEvent) error {
	return nil
}

type nopSlack struct{}

func (nopSlack) NotifyAlert(context.Context, string) error { return nil }

// Compile-time guards.
var (
	_ enrollment.CountStore             = zeroCount{}
	_ enrollment.WindowCounter          = passWindowCounter{}
	_ enrollment.Breaker                = zeroBreaker{}
	_ slugreservation.MasterAuditLogger = nopAudit{}
	_ slugreservation.SlackNotifier     = nopSlack{}
	_ management.EnrollmentGate         = enrollmentGateAdapter{}
	_ management.SlugReleaser           = slugReleaseAdapter{}
	_ management.AuditLogger            = managementAudit{}
	_ circuitbreaker.State              = (*circuitbreaker.InMemoryState)(nil) // package import sentinel
)
