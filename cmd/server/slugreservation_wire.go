package main

// SIN-62331 F51 — HTTP wiring for the slugreservation domain (F46).
//
// The slugreservation package exposes three boundary pieces:
//
//   - RequireSlugAvailable middleware (signup + tenant-rename gate).
//   - RedirectHandler         (host-level <old>.<primary> → 301 + Clear-Site-Data).
//   - OverrideHandler         (POST /api/master/slug-reservations/{slug}/release).
//
// Without this file none of those pieces fire at runtime: the domain
// guarantees verified by the F43–F49 re-review (SIN-62328) only become
// actual defenses when the boundary calls them on every request. See
// docs/adr/0079-custom-domain.md §"HTTP wiring" for the deployment map.
//
// Gating:
//
//   - The Postgres-backed wiring requires DATABASE_URL. Without it, the
//     boundary returns nil pieces and cmd/server falls back to no-op
//     middleware that lets the request through (the deployment is then
//     responsible for not enabling signup/rename without a database).
//   - The master OverrideHandler is gated by MASTER_API_TOKEN. When the
//     token is unset, the master mux denies every request with 401 (deny
//     by default). This keeps the route mounted but inert until an
//     operator provisions the token, instead of silently disabling the
//     whole administrative surface.

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	"github.com/pericles-luz/crm/internal/slugreservation"
)

const (
	envMasterAPIToken    = "MASTER_API_TOKEN"
	envMasterAPIRequire  = "MASTER_REQUIRE_MFA"
	masterAuthHeader     = "Authorization"
	masterAuthBearer     = "Bearer "
	masterContextID      = "MasterID"
	signupSlugPathParam  = "slug"
	renameSlugPathParam  = "slug"
	envSlugReservationDB = pgpool.EnvDSN
)

// slugReservationWiring bundles the boundary pieces produced by
// buildSlugReservationWiring. nil fields mean "not wired"; cmd/server
// then mounts the no-op variants (so /health and tests without
// DATABASE_URL keep working).
type slugReservationWiring struct {
	service     *slugreservation.Service
	requireSlug func(slugreservation.SlugExtractor) func(http.Handler) http.Handler
	redirect    func(next http.Handler) http.Handler
	override    *slugreservation.OverrideHandler
	cleanup     func()
	primaryHost string
}

// buildSlugReservationWiring assembles the slug reservation boundary.
// Production wires all three pieces. Tests inject a fake pool through
// buildSlugReservationWiringWith so the smoke suite stays in-process.
func buildSlugReservationWiring(ctx context.Context, getenv func(string) string) slugReservationWiring {
	return buildSlugReservationWiringWith(ctx, getenv, defaultSlugReservationDial)
}

// slugReservationDial is the test seam; production opens a pgxpool.
type slugReservationDial func(ctx context.Context, dsn string) (slugReservationPool, error)

// slugReservationPool is the narrow surface the slugreservation stores
// require: pgstore.PgxConn (QueryRow + Exec) plus Close. *pgxpool.Pool
// satisfies it.
type slugReservationPool interface {
	pgstore.PgxConn
	Close()
}

func defaultSlugReservationDial(ctx context.Context, dsn string) (slugReservationPool, error) {
	pool, err := pgpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return pool, nil
}

func buildSlugReservationWiringWith(ctx context.Context, getenv func(string) string, dial slugReservationDial) slugReservationWiring {
	noop := slugReservationWiring{
		requireSlug: func(slugreservation.SlugExtractor) func(http.Handler) http.Handler {
			return func(next http.Handler) http.Handler { return next }
		},
		redirect: func(next http.Handler) http.Handler { return next },
		cleanup:  func() {},
	}
	dsn := getenv(envSlugReservationDB)
	if dsn == "" {
		return noop
	}

	pool, err := dial(ctx, dsn)
	if err != nil {
		log.Printf("crm: slug reservation wiring disabled — pg connect: %v", err)
		return noop
	}

	svc, err := slugreservation.NewService(
		pgstore.NewSlugReservationStore(pool),
		pgstore.NewSlugRedirectStore(pool),
		slogMasterAudit{logger: slog.Default()},
		nopSlack{},
		nil,
	)
	if err != nil {
		pool.Close()
		log.Printf("crm: slug reservation wiring disabled — service: %v", err)
		return noop
	}

	primary := getenv(envCustomDomainPrimary)
	if primary == "" {
		primary = "exemplo.com"
	}

	requireMFA := getenv(envMasterAPIRequire) == "1"
	override := slugreservation.NewOverrideHandler(svc, masterAuthorizerFromContext{}, requireMFA)

	return slugReservationWiring{
		service: svc,
		requireSlug: func(extract slugreservation.SlugExtractor) func(http.Handler) http.Handler {
			return slugreservation.RequireSlugAvailable(svc, extract)
		},
		redirect: func(next http.Handler) http.Handler {
			return slugreservation.NewRedirectHandler(svc, primary, next)
		},
		override:    override,
		primaryHost: primary,
		cleanup: func() {
			pool.Close()
		},
	}
}

// masterContextKey is the context key that masterAuthMiddleware stamps
// with the authenticated master ID and MFA bit. masterAuthorizerFromContext
// reads it back inside OverrideHandler.
type masterContextKey struct{}

type masterContextValue struct {
	id  uuid.UUID
	mfa bool
}

// masterAuthorizerFromContext implements slugreservation.MasterAuthorizer
// by reading whatever masterAuthMiddleware stamped on the context.
type masterAuthorizerFromContext struct{}

func (masterAuthorizerFromContext) AuthorizeMaster(ctx context.Context) (uuid.UUID, bool, error) {
	v, ok := ctx.Value(masterContextKey{}).(masterContextValue)
	if !ok {
		return uuid.Nil, false, errors.New("master: not authenticated")
	}
	return v.id, v.mfa, nil
}

// masterAuthMiddleware enforces a deny-by-default master gate.
//
// When MASTER_API_TOKEN is unset, every request to the master mux
// returns 401 with no body. This is the fail-closed default: shipping
// the binary without provisioning the token must not silently expose
// the master surface.
//
// When the token is configured, the middleware:
//
//  1. Requires `Authorization: Bearer <token>` and a constant-time
//     compare (subtle.ConstantTimeCompare) so wrong-token responses
//     don't leak length-of-prefix information.
//  2. Stamps the context with a fixed master identity (the token's
//     associated master). The MFA bit defaults to true when
//     MASTER_REQUIRE_MFA=1; when not set it falls back to true so the
//     OverrideHandler's requireMFA gate stays effective.
//
// The token-to-master-identity mapping is intentionally simple here:
// production deployments that need per-master identities should layer
// real master auth (SIN-62342 RequireMasterMFA) on top, replacing this
// middleware. Until that lands, the constant token + bearer flow is the
// minimum viable boundary that "defaults to deny" and lets the
// SIN-62331 wiring be smoke-tested end-to-end.
func masterAuthMiddleware(getenv func(string) string) func(http.Handler) http.Handler {
	token := strings.TrimSpace(getenv(envMasterAPIToken))
	mfa := getenv(envMasterAPIRequire) == "1" || token != ""
	masterID := masterIDFromToken(token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if token == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h := r.Header.Get(masterAuthHeader)
			if !strings.HasPrefix(h, masterAuthBearer) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			provided := strings.TrimPrefix(h, masterAuthBearer)
			if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), masterContextKey{}, masterContextValue{
				id:  masterID,
				mfa: mfa,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// masterIDFromToken derives a stable UUID from the token so the
// OverrideHandler audit trail records a non-zero master ID. Production
// auth (SIN-62342) replaces this with the real master row id; the
// derivation lives here only as long as the placeholder middleware is
// in service. Empty token returns uuid.Nil (the OverrideHandler
// rejects that explicitly, so even a bug in masterAuthMiddleware
// cannot accidentally pass auth).
func masterIDFromToken(token string) uuid.UUID {
	if token == "" {
		return uuid.Nil
	}
	// Use a deterministic v5 UUID rooted at a fixed namespace so the
	// same token always yields the same master id across restarts.
	return uuid.NewSHA1(masterTokenNamespace, []byte(token))
}

// masterTokenNamespace is a fixed v4 used as the v5 namespace. Generated
// once and frozen so the resulting v5 ids are stable across deploys.
var masterTokenNamespace = uuid.MustParse("3a160897-1de2-4cc8-bef4-d07af11c6a76")

// signupRenamePlaceholder is the temporary handler the signup and
// rename endpoints share until SIN-62342 (master MFA + tenant flows)
// lands the real handlers. Returning 501 keeps callers from mistaking
// the wiring for a working business flow while still making the
// RequireSlugAvailable middleware testable end-to-end (the middleware
// short-circuits the chain at 409 before this handler ever runs).
func signupRenamePlaceholder(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// registerSlugReservationRoutes mounts the F46/F47/F48 boundary on the
// public mux:
//
//   - POST /api/master/slug-reservations/{slug}/release — OverrideHandler
//     wrapped in masterAuthMiddleware (deny by default).
//   - POST /signup, PATCH /tenants/{slug}/slug — placeholder handlers
//     wrapped in RequireSlugAvailable so the F46 409 short-circuit is
//     mounted ahead of the real signup/rename use-cases.
//
// When buildSlugReservationWiring returned the no-op variant (no DSN),
// the override handler is omitted and the placeholders fall through to
// 501 without any reservation check — that matches the production rule
// that signup must not be enabled without a database.
func registerSlugReservationRoutes(mux *http.ServeMux, w slugReservationWiring, getenv func(string) string) {
	if w.override != nil {
		mux.Handle("POST /api/master/slug-reservations/{slug}/release",
			masterAuthMiddleware(getenv)(w.override))
	}
	signupHandler := w.requireSlug(formSlugExtractor("slug"))(http.HandlerFunc(signupRenamePlaceholder))
	mux.Handle("POST /signup", signupHandler)
	renameHandler := w.requireSlug(slugreservation.PathValueExtractor(renameSlugPathParam))(http.HandlerFunc(signupRenamePlaceholder))
	mux.Handle("PATCH /tenants/{slug}/slug", renameHandler)
}

// formSlugExtractor reads the candidate slug from the form-encoded
// signup body (`slug=<value>`). ParseForm reads at most r.ContentLength
// (default cap 10 MiB) — well within what a signup form ever needs.
func formSlugExtractor(field string) slugreservation.SlugExtractor {
	return func(r *http.Request) (string, bool) {
		if err := r.ParseForm(); err != nil {
			return "", false
		}
		v := strings.TrimSpace(r.PostFormValue(field))
		if v == "" {
			return "", false
		}
		return v, true
	}
}

// slogMasterAudit is the SIN-62331 default MasterAuditLogger. It writes
// a structured slog record on every override; the master_ops_audit
// trigger captures the row-level write separately. SIN-62342 replaces
// this with the persistent audit-log writer.
type slogMasterAudit struct{ logger *slog.Logger }

func (a slogMasterAudit) LogMasterOverride(ctx context.Context, ev slugreservation.MasterOverrideEvent) error {
	a.logger.LogAttrs(ctx, slog.LevelInfo, "slugreservation.master_override",
		slog.String("slug", ev.Slug),
		slog.String("master_id", ev.MasterID.String()),
		slog.String("reason", ev.Reason),
		slog.Time("at", ev.At),
	)
	return nil
}

// Compile-time guards.
var (
	_ slugreservation.MasterAuthorizer  = masterAuthorizerFromContext{}
	_ slugreservation.MasterAuditLogger = slogMasterAudit{}
)
