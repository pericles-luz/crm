// Package httpapi assembles the chi router that exposes the CRM HTTP
// surface. It is the seam between cmd/server (which owns wireup) and the
// per-route handlers; it never touches the database, iam internals, or
// the filesystem directly.
//
// Middleware order is load-bearing (SIN-62217 §Middleware chain):
//
//	RequestID → RealIP → Logger → Recoverer → TenantScope → Auth
//
// Each link layers on the next; reordering breaks security guarantees
// (e.g. Auth without TenantScope cannot validate per-tenant sessions).
//
// Routing layout:
//
//	GET  /health          — liveness (NO tenant scope, NO auth)
//	GET  /login           — render form          (tenant scope, no auth)
//	POST /login           — submit credentials    (tenant scope, no auth)
//	GET  /logout          — clear session cookie  (tenant scope, no auth)
//	GET  /hello-tenant    — protected page        (tenant scope + auth)
package httpapi

import (
	"context"
	"log/slog"
	"net"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/handler"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// IAMService is the union of iam.Service slices the handlers need. The
// concrete *iam.Service satisfies it; tests inject fakes implementing the
// same shape.
type IAMService interface {
	Login(ctx context.Context, host, email, password string, ipAddr net.IP, userAgent string) (iam.Session, error)
	Logout(ctx context.Context, tenantID, sessionID uuid.UUID) error
	ValidateSession(ctx context.Context, tenantID, sessionID uuid.UUID) (iam.Session, error)
}

// Deps is the constructor-injected dependency bag for NewRouter. cmd/server
// builds it once at bootstrap; tests build it with fakes.
type Deps struct {
	IAM            IAMService
	TenantResolver tenancy.Resolver
	Logger         *slog.Logger
	// CookieSecure flips the Secure attribute on the session cookie.
	// Production MUST set true. Dev/integration tests pass false so a
	// plaintext httptest.Server can read the cookie back.
	CookieSecure bool
}

// NewRouter wires the chi router with the canonical middleware chain and
// the SIN-62217 routes. Handlers and middleware are stitched here; the
// individual files in handler/ and middleware/ have no awareness of each
// other's existence beyond the small ports they export.
func NewRouter(deps Deps) http.Handler {
	if deps.IAM == nil {
		panic("httpapi: Deps.IAM is nil")
	}
	if deps.TenantResolver == nil {
		panic("httpapi: Deps.TenantResolver is nil")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}

	r := chi.NewRouter()

	// Cross-cutting middleware. Order is fixed by SIN-62217 §Middleware
	// chain — never reorder without an ADR.
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(slogRequestLogger(deps.Logger))
	r.Use(chimw.Recoverer)

	// /health is the only route that bypasses the tenant scope. The LB
	// must reach it without resolving a tenant by host (the host might
	// be the raw load-balancer DNS name).
	r.Get("/health", handler.Health)

	// All other routes go through TenantScope. Public-but-tenanted
	// routes (login, logout) live in this group; the authenticated
	// subset is nested below.
	r.Group(func(tenanted chi.Router) {
		tenanted.Use(middleware.TenantScope(deps.TenantResolver))
		tenanted.Get("/login", handler.LoginGet)
		tenanted.Post("/login", handler.LoginPost(handler.LoginConfig{
			IAM:          deps.IAM,
			CookieSecure: deps.CookieSecure,
		}))
		tenanted.Get("/logout", handler.Logout(deps.IAM))

		tenanted.Group(func(authed chi.Router) {
			authed.Use(middleware.Auth(deps.IAM))
			authed.Get("/hello-tenant", handler.HelloTenant)
		})
	})

	return r
}

// slogRequestLogger emits one structured log line per request. It is
// intentionally minimal: it logs status, method, path, and request id —
// nothing that depends on a body parse, since that would interfere with
// the body-form interop pattern documented on handler.LoginConfig.
func slogRequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			logger.LogAttrs(r.Context(), slog.LevelInfo, "http: request",
				slog.String("request_id", chimw.GetReqID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
			)
		})
	}
}
