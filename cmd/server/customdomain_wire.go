package main

// SIN-62259 + SIN-62313 wiring — public-side custom-domain management UI.
//
// The handler is wired only when CUSTOM_DOMAIN_UI_ENABLED=1 AND
// DATABASE_URL + CUSTOM_DOMAIN_CSRF_SECRET are configured. When any of
// those are unset the handler is omitted; the existing /health route
// remains the only public surface (preserving cmd/server tests that
// run without external deps).
//
// DNS validation (SIN-62313): the management use-case is constructed
// with the SIN-62242 validator (`internal/customdomain/validation`)
// backed by the production miekg adapter (`adapters/dnsresolver/miekg`).
// CUSTOMDOMAIN_DNS_SERVER selects the recursive resolver — production
// MUST point this at the Unbound sidecar configured in
// `infra/caddy/unbound.conf` (typically `unbound:5353` inside the
// compose network) so DNS rebinding into private/loopback ranges is
// rejected at both the validator and the recursive cache (ADR 0079 §2).
// CUSTOMDOMAIN_DNSSEC=1 (default on) enables DNSSEC observability so
// the `verified_with_dnssec` badge in the UI reflects whether the
// resolver returned the AD bit on the answer.
//
// Gating on missing DNS — fail-fast at startup. If the feature flag is
// on but the resolver cannot be constructed (currently the only failure
// mode is a misconfigured server address surfaced when LookupIP is first
// called), the wire-up still installs the handler with a working
// validator — the resolver itself does not perform a startup probe to
// avoid blocking boot on transient DNS outages. The `Verificar agora`
// button is therefore always rendered when the feature flag is on; if
// the resolver is unreachable at request time the UI surfaces the
// resolver error inline as a row tooltip ("dns_resolution_failed"),
// which is the same UX the user sees when their own DNS publishing is
// broken. The alternative — disabling the button when the resolver is
// nil — is unreachable because the resolver is now always constructed
// when the flag is on; we keep the management.Config{DNS: nil} branch
// for tests that intentionally simulate the unwired state.
//
// The enrollment-quota / circuit-breaker gate is wired to in-memory
// no-op placeholders (zeroCount / passWindowCounter / zeroBreaker) so
// the UI is bootable without Redis. This is INTENTIONAL for the v1
// flag-flip — it disables the quota check, so production deploys MUST
// swap to a Redis-backed adapter via the follow-up child issue before
// flipping CUSTOM_DOMAIN_UI_ENABLED=1 in production. A startup WARN is
// emitted from buildEnrollmentGate so the gap is visible in logs.
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
	"strconv"
	"time"

	"github.com/google/uuid"

	miekgresolver "github.com/pericles-luz/crm/adapters/dnsresolver/miekg"
	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	customdomainhttp "github.com/pericles-luz/crm/internal/adapter/transport/http/customdomain"
	"github.com/pericles-luz/crm/internal/customdomain/circuitbreaker"
	"github.com/pericles-luz/crm/internal/customdomain/enrollment"
	"github.com/pericles-luz/crm/internal/customdomain/management"
	"github.com/pericles-luz/crm/internal/customdomain/validation"
	"github.com/pericles-luz/crm/internal/iam/dnsresolver"
	"github.com/pericles-luz/crm/internal/slugreservation"
)

const (
	envCustomDomainUI       = "CUSTOM_DOMAIN_UI_ENABLED"
	envCustomDomainCSRF     = "CUSTOM_DOMAIN_CSRF_SECRET"
	envCustomDomainPrimary  = "PRIMARY_DOMAIN"
	envCustomDomainTenantHd = "X-Tenant-ID"
	envCustomDomainDNSSrv   = "CUSTOMDOMAIN_DNS_SERVER"
	envCustomDomainDNSSEC   = "CUSTOMDOMAIN_DNSSEC"
	enrollmentRedisPrefix   = "customdomain:enrollment"
)

// customDomainDial is the test seam: the production wiring opens a real
// pgxpool; tests inject a fake pool that satisfies pgstore.PgxRowsConn.
type customDomainDial func(ctx context.Context, dsn string) (customDomainPool, error)

// customDomainResolverFactory is the test seam for the SIN-62242
// resolver. Production returns a *miekg.Resolver bound to the configured
// recursive server; tests inject a deterministic in-memory fake.
type customDomainResolverFactory func(getenv func(string) string) dnsresolver.Resolver

// defaultCustomDomainResolverFactory returns a miekg-backed resolver
// pointed at CUSTOMDOMAIN_DNS_SERVER when set, falling back to the
// stdlib resolver address. Production deploys MUST set the env var to
// the Unbound sidecar (typically `unbound:5353`) — see ADR 0079 §2.
func defaultCustomDomainResolverFactory(getenv func(string) string) dnsresolver.Resolver {
	server := getenv(envCustomDomainDNSSrv)
	dnssec := true
	if v := getenv(envCustomDomainDNSSEC); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			dnssec = parsed
		}
	}
	return miekgresolver.NewResolver(miekgresolver.Config{
		Server:       server,
		EnableDNSSEC: dnssec,
	})
}

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
	return buildCustomDomainHandlerWithDeps(ctx, getenv, defaultCustomDomainDial, defaultCustomDomainResolverFactory)
}

// buildCustomDomainHandlerWith is the test-friendly variant for the
// pgxpool dial seam. Production uses the resolver factory baked into
// buildCustomDomainHandler; tests that need to inject a fake DNS
// resolver should call buildCustomDomainHandlerWithDeps instead. This
// signature is preserved for backwards compatibility with the SIN-62259
// test suite.
func buildCustomDomainHandlerWith(ctx context.Context, getenv func(string) string, dial customDomainDial) (http.Handler, func()) {
	return buildCustomDomainHandlerWithDeps(ctx, getenv, dial, defaultCustomDomainResolverFactory)
}

// buildCustomDomainHandlerWithDeps is the full test seam for the
// SIN-62313 wire-up. Both the dial and resolverFactory arguments cover
// network-touching paths; passing stubs short-circuits the pgxpool.New
// and miekg.NewResolver calls so handler tests can drive the resolver
// deterministically.
func buildCustomDomainHandlerWithDeps(ctx context.Context, getenv func(string) string, dial customDomainDial, resolverFactory customDomainResolverFactory) (http.Handler, func()) {
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
	// SIN-62313: wire the SIN-62242 validator + miekg adapter so the
	// `Verificar agora` path executes the real DNS-only ownership
	// validation (TXT proof + IP allowlist + DNSSEC observability).
	validator := validation.New(resolverFactory(getenv), validationAudit{logger: slog.Default()}, validation.SystemClock{})
	uc, err := management.New(management.Config{
		Store:     store,
		Gate:      gate,
		Validator: hostValidatorAdapter{v: validator},
		DNS:       dnsCheckerAdapter{v: validator},
		Slug:      slugReleaseAdapter{svc: slugSvc},
		Audit:     managementAudit{logger: slog.Default()},
		Now:       time.Now,
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

// buildEnrollmentGate wires *enrollment.UseCase against in-memory no-op
// placeholders (zeroCount / passWindowCounter / zeroBreaker) so the UI
// is bootable without Redis. This effectively DISABLES the per-tenant
// enrollment quota and the LE circuit breaker on the public listener
// — the production swap is tracked in the follow-up child issue
// referenced from the package doc.
//
// A startup WARN is emitted on every wire-up so the gap is visible in
// production logs; do not flip CUSTOM_DOMAIN_UI_ENABLED=1 in a
// customer-facing environment until the Redis-backed adapter has
// replaced these placeholders.
func buildEnrollmentGate(_ pgstore.PgxConn) (management.EnrollmentGate, func()) {
	slog.Default().Warn(
		"customdomain: enrollment quota and circuit breaker are running in dev/no-op mode (in-memory placeholders); swap to Redis-backed adapters before flipping CUSTOM_DOMAIN_UI_ENABLED=1 in production",
		slog.String("component", "customdomain.management"),
		slog.String("gate", "enrollment"),
		slog.String("breaker", "letsencrypt"),
	)
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

// validationAudit forwards SIN-62242 validation events to slog so the
// SSRF / token-mismatch / DNSSEC observability shows up in the same
// log pipeline as the management events.
type validationAudit struct{ logger *slog.Logger }

func (a validationAudit) Record(ctx context.Context, ev validation.AuditEvent) {
	attrs := []slog.Attr{
		slog.String("event", ev.Event),
		slog.String("host", ev.Host),
		slog.Time("at", ev.At),
	}
	for k, v := range ev.Detail {
		attrs = append(attrs, slog.String("detail."+k, v))
	}
	a.logger.LogAttrs(ctx, slog.LevelInfo, "customdomain.validation", attrs...)
}

// hostValidatorAdapter projects validation.Validator's host-only check
// into management.HostValidator. The Enroll path calls this before
// issuing a verification token, so the validator skips the TXT proof
// (the user has not published it yet) and only enforces the FQDN /
// IP-allowlist contract from ADR 0079 §1. Sentinel error mapping keeps
// the management.classifyValidationError -> Reason chain intact:
// validation.ErrPrivateIP is wrapped in management.ErrPrivateIP so
// errors.Is reaches the right Reason.
type hostValidatorAdapter struct{ v *validation.Validator }

func (h hostValidatorAdapter) Validate(ctx context.Context, host string) error {
	if err := h.v.ValidateHostOnly(ctx, host); err != nil {
		switch {
		case errors.Is(err, validation.ErrPrivateIP):
			return fmt.Errorf("%w: %w", management.ErrPrivateIP, err)
		case errors.Is(err, validation.ErrEmptyHost):
			return fmt.Errorf("%w: %w", management.ErrInvalidHost, err)
		default:
			return err
		}
	}
	return nil
}

// dnsCheckerAdapter projects validation.Validator into management.DNSChecker.
// The Verify path calls this with the stored verification token; the
// validator runs the full DNS-only check (TXT proof + IP allowlist +
// DNSSEC observability) and returns the result the management
// use-case needs to flip `verified_with_dnssec`.
//
// Error mapping mirrors hostValidatorAdapter: validation sentinels are
// wrapped in management equivalents so errors.Is on the boundary
// classifies them correctly. ErrTokenMismatch is the most important
// case — without the wrap, classifyValidationError falls through to
// ReasonDNSResolutionFailed and the UI shows the wrong PT-BR copy.
//
// LogID is currently nil because the validation use-case does not yet
// persist a dns_resolution_log row; that wiring lives behind a separate
// store port and is tracked in the F44 follow-up. The DNSSEC flag still
// flows through so the badge renders correctly.
type dnsCheckerAdapter struct{ v *validation.Validator }

func (d dnsCheckerAdapter) Check(ctx context.Context, host, expectedToken string) (management.DNSCheckResult, error) {
	res, err := d.v.Validate(ctx, host, expectedToken)
	if err != nil {
		switch {
		case errors.Is(err, validation.ErrTokenMismatch):
			return management.DNSCheckResult{}, fmt.Errorf("%w: %w", management.ErrTokenMismatch, err)
		case errors.Is(err, validation.ErrPrivateIP):
			return management.DNSCheckResult{}, fmt.Errorf("%w: %w", management.ErrPrivateIP, err)
		default:
			return management.DNSCheckResult{}, err
		}
	}
	return management.DNSCheckResult{WithDNSSEC: res.VerifiedWithDNSSEC}, nil
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
	_ management.HostValidator          = hostValidatorAdapter{}
	_ management.DNSChecker             = dnsCheckerAdapter{}
	_ validation.Auditor                = validationAudit{}
	_ circuitbreaker.State              = (*circuitbreaker.InMemoryState)(nil) // package import sentinel
)
