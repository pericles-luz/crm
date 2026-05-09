package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pericles-luz/crm/internal/slugreservation"
)

// fakeSlugRow lets the fake pool answer a single QueryRow with a
// preset slug reservation row for a given slug, or pgx.ErrNoRows if
// the slug doesn't match.
type fakeSlugRow struct {
	slug string
	hit  *slugreservation.Reservation
}

func (r fakeSlugRow) Scan(dest ...any) error {
	if r.hit == nil {
		return pgx.ErrNoRows
	}
	// scanReservation in slug_reservation_store.go scans:
	//   *[16]byte, *string, *time.Time, **[16]byte, *time.Time, *time.Time
	// for (id, slug, released_at, released_by_tenant_id, expires_at, created_at).
	// Mirror that shape exactly.
	*(dest[0].(*[16]byte)) = [16]byte(r.hit.ID)
	*(dest[1].(*string)) = r.hit.Slug
	*(dest[2].(*time.Time)) = r.hit.ReleasedAt
	if rb, ok := dest[3].(**[16]byte); ok {
		*rb = nil // released_by tenant id NULL — fine for the smoke test.
	}
	*(dest[4].(*time.Time)) = r.hit.ExpiresAt
	*(dest[5].(*time.Time)) = r.hit.CreatedAt
	return nil
}

// fakeSlugPool drives the slug reservation store with a single reserved
// slug. Other slug lookups return pgx.ErrNoRows so they fall through to
// "available". Insert and SoftDelete return success without persisting
// anything; the smoke test only exercises the boundary, not the store.
type fakeSlugPool struct {
	reserved string
	closed   bool
}

func (f *fakeSlugPool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if len(args) == 0 {
		return errSlugRow{err: pgx.ErrNoRows}
	}
	slug, ok := args[0].(string)
	if !ok {
		return errSlugRow{err: pgx.ErrNoRows}
	}
	switch {
	case strings.Contains(sql, "FROM tenant_slug_reservation") && slug == f.reserved:
		return fakeSlugRow{slug: slug, hit: &slugreservation.Reservation{
			ID:        uuid.New(),
			Slug:      slug,
			ExpiresAt: time.Now().Add(48 * time.Hour),
		}}
	case strings.Contains(sql, "FROM tenant_slug_redirect"):
		// Redirect lookup not exercised in this smoke test; return no rows.
		return errSlugRow{err: pgx.ErrNoRows}
	default:
		return errSlugRow{err: pgx.ErrNoRows}
	}
}

func (f *fakeSlugPool) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (f *fakeSlugPool) Close() { f.closed = true }

type errSlugRow struct{ err error }

func (r errSlugRow) Scan(_ ...any) error { return r.err }

func newSlugWiringForTest(t *testing.T, reserved string, getenv func(string) string) (slugReservationWiring, *fakeSlugPool) {
	t.Helper()
	pool := &fakeSlugPool{reserved: reserved}
	dial := func(_ context.Context, _ string) (slugReservationPool, error) {
		return pool, nil
	}
	w := buildSlugReservationWiringWith(context.Background(), getenv, dial)
	return w, pool
}

func TestBuildSlugReservationWiring_NoDSN_ReturnsNoOps(t *testing.T) {
	t.Parallel()
	w := buildSlugReservationWiring(context.Background(), func(string) string { return "" })
	defer w.cleanup()
	if w.service != nil {
		t.Fatal("service should be nil when DSN unset")
	}
	if w.override != nil {
		t.Fatal("override should be nil when DSN unset")
	}
	// No-op middleware/redirect must still be callable.
	wrapped := w.requireSlug(slugreservation.PathValueExtractor("slug"))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("no-op middleware should pass through, got status %d", rec.Code)
	}
}

func TestSignupRoute_ReservedSlug_Returns409(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envSlugReservationDB:
			return "postgres://example/db"
		}
		return ""
	}
	w, _ := newSlugWiringForTest(t, "taken", getenv)
	defer w.cleanup()
	mux := http.NewServeMux()
	registerSlugReservationRoutes(mux, w, getenv)

	body := strings.NewReader("slug=taken")
	r := httptest.NewRequest(http.MethodPost, "/signup", body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want JSON", got)
	}
}

func TestSignupRoute_FreeSlug_Returns501Placeholder(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == envSlugReservationDB {
			return "postgres://example/db"
		}
		return ""
	}
	w, _ := newSlugWiringForTest(t, "different", getenv)
	defer w.cleanup()
	mux := http.NewServeMux()
	registerSlugReservationRoutes(mux, w, getenv)

	r := httptest.NewRequest(http.MethodPost, "/signup", strings.NewReader("slug=free"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d (placeholder)", rec.Code, http.StatusNotImplemented)
	}
}

func TestRenameRoute_ReservedSlug_Returns409(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == envSlugReservationDB {
			return "postgres://example/db"
		}
		return ""
	}
	w, _ := newSlugWiringForTest(t, "taken", getenv)
	defer w.cleanup()
	mux := http.NewServeMux()
	registerSlugReservationRoutes(mux, w, getenv)

	r := httptest.NewRequest(http.MethodPatch, "/tenants/taken/slug", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestMasterOverride_NoToken_DenyByDefault(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == envSlugReservationDB {
			return "postgres://example/db"
		}
		return ""
	}
	w, _ := newSlugWiringForTest(t, "", getenv)
	defer w.cleanup()
	mux := http.NewServeMux()
	registerSlugReservationRoutes(mux, w, getenv)

	r := httptest.NewRequest(http.MethodPost, "/api/master/slug-reservations/foo/release", strings.NewReader(`{"reason":"qa"}`))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (deny by default)", rec.Code, http.StatusUnauthorized)
	}
}

func TestMasterOverride_WrongToken_Returns401(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envSlugReservationDB:
			return "postgres://example/db"
		case envMasterAPIToken:
			return "secret-token"
		}
		return ""
	}
	w, _ := newSlugWiringForTest(t, "", getenv)
	defer w.cleanup()
	mux := http.NewServeMux()
	registerSlugReservationRoutes(mux, w, getenv)

	r := httptest.NewRequest(http.MethodPost, "/api/master/slug-reservations/foo/release", strings.NewReader(`{"reason":"qa"}`))
	r.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestMasterOverride_ValidToken_PassesAuth verifies that with a
// matching bearer token, the request reaches the OverrideHandler.
// Since the fake pool's Exec returns success for SoftDelete but
// QueryRow returns ErrNoRows on a slug that's not reserved, the
// service path returns ErrNotReserved → 404. That's still proof the
// auth gate let the request through.
func TestMasterOverride_ValidToken_ReachesHandler(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envSlugReservationDB:
			return "postgres://example/db"
		case envMasterAPIToken:
			return "secret-token"
		}
		return ""
	}
	w, _ := newSlugWiringForTest(t, "", getenv)
	defer w.cleanup()
	mux := http.NewServeMux()
	registerSlugReservationRoutes(mux, w, getenv)

	r := httptest.NewRequest(http.MethodPost, "/api/master/slug-reservations/foo/release", strings.NewReader(`{"reason":"manual qa"}`))
	r.Header.Set("Authorization", "Bearer secret-token")
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	// Auth passes (status != 401/403). Body path may 404/500 because
	// the fake pool's Exec returns 0 rows for SoftDelete; we only
	// assert the auth gate let the request through.
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("auth should succeed with matching token, got %d", rec.Code)
	}
}

// TestRedirectHandler_OldSlug_Returns301 verifies the redirect
// handler fires for a request whose Host is `<old>.<primary>`.
// We bypass the slugreservation Service to avoid wiring the redirect
// store and instead test the wrapper produced by buildSlugReservationWiring.
func TestRedirectHandler_OldSlug_FiresOnHost(t *testing.T) {
	t.Parallel()
	// We cannot easily wire a redirect hit with the fake pool because
	// the Active query is independent of the slug reservation
	// QueryRow above; instead we drive a direct test against the
	// slugreservation.NewRedirectHandler with a Service backed by an
	// in-memory redirect store stub.
	svc := mustService(t, &memSlugRedirectStore{m: map[string]string{"old": "new"}})
	h := slugreservation.NewRedirectHandler(svc, "exemplo.com", http.NotFoundHandler())
	r := httptest.NewRequest(http.MethodGet, "/path?q=1", nil)
	r.Host = "old.exemplo.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMovedPermanently)
	}
	if got := rec.Header().Get("Clear-Site-Data"); got != slugreservation.ClearSiteDataCookies {
		t.Fatalf("Clear-Site-Data = %q, want %q", got, slugreservation.ClearSiteDataCookies)
	}
	if got := rec.Header().Get("Location"); !strings.Contains(got, "new.exemplo.com") {
		t.Fatalf("Location = %q, want host new.exemplo.com", got)
	}
}

func TestRedirectHandler_PrimaryHost_PassesThrough(t *testing.T) {
	t.Parallel()
	svc := mustService(t, &memSlugRedirectStore{m: map[string]string{}})
	called := false
	h := slugreservation.NewRedirectHandler(svc, "exemplo.com", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	r.Host = "exemplo.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if !called {
		t.Fatal("primary host must pass through to next")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestMasterIDFromToken_Stable confirms a non-empty token always maps
// to the same v5 UUID across calls — required for the audit trail's
// master_id consistency across restarts.
func TestMasterIDFromToken_Stable(t *testing.T) {
	t.Parallel()
	a := masterIDFromToken("alpha")
	b := masterIDFromToken("alpha")
	if a != b {
		t.Fatalf("masterIDFromToken not stable: %s vs %s", a, b)
	}
	if a == uuid.Nil {
		t.Fatal("masterIDFromToken returned uuid.Nil for non-empty token")
	}
	if masterIDFromToken("") != uuid.Nil {
		t.Fatal("masterIDFromToken should return uuid.Nil for empty token")
	}
}

func TestSignupRoute_NoDSN_FallsThroughTo501(t *testing.T) {
	t.Parallel()
	w := buildSlugReservationWiring(context.Background(), func(string) string { return "" })
	defer w.cleanup()
	mux := http.NewServeMux()
	registerSlugReservationRoutes(mux, w, func(string) string { return "" })

	r := httptest.NewRequest(http.MethodPost, "/signup", strings.NewReader("slug=anything"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotImplemented)
	}
}

// memSlugRedirectStore is an in-memory slugreservation.RedirectStore for
// the redirect-handler smoke test. It keeps the test self-contained
// without spinning up Postgres.
type memSlugRedirectStore struct{ m map[string]string }

func (s *memSlugRedirectStore) Active(_ context.Context, oldSlug string) (slugreservation.Redirect, error) {
	v, ok := s.m[oldSlug]
	if !ok {
		return slugreservation.Redirect{}, slugreservation.ErrNotReserved
	}
	return slugreservation.Redirect{
		OldSlug:   oldSlug,
		NewSlug:   v,
		ExpiresAt: time.Now().Add(48 * time.Hour),
	}, nil
}

func (s *memSlugRedirectStore) Upsert(_ context.Context, oldSlug, newSlug string, expiresAt time.Time) (slugreservation.Redirect, error) {
	s.m[oldSlug] = newSlug
	return slugreservation.Redirect{OldSlug: oldSlug, NewSlug: newSlug, ExpiresAt: expiresAt}, nil
}

// memReservationStore is the no-op slugreservation.Store the redirect
// handler test needs. The redirect handler only calls LookupRedirect,
// so Active/Insert/SoftDelete only need to compile.
type memReservationStore struct{}

func (memReservationStore) Active(context.Context, string) (slugreservation.Reservation, error) {
	return slugreservation.Reservation{}, slugreservation.ErrNotReserved
}
func (memReservationStore) Insert(context.Context, string, uuid.UUID, time.Time, time.Time) (slugreservation.Reservation, error) {
	return slugreservation.Reservation{}, errors.New("not implemented")
}
func (memReservationStore) SoftDelete(context.Context, string, time.Time) (slugreservation.Reservation, error) {
	return slugreservation.Reservation{}, slugreservation.ErrNotReserved
}

func mustService(t *testing.T, redirects slugreservation.RedirectStore) *slugreservation.Service {
	t.Helper()
	svc, err := slugreservation.NewService(memReservationStore{}, redirects, slogMasterAudit{logger: slog.Default()}, nopSlack{}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// makeTinyPNG returns a 1x1 PNG byte stream the upload smoke test
// uses to exercise the success path.
func makeTinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 0xFF, G: 0xAA, B: 0x55, A: 0xFF})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func TestMasterAuthMiddleware_StampsContextOnHit(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == envMasterAPIToken {
			return "secret"
		}
		return ""
	}
	called := false
	authed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, mfa, err := masterAuthorizerFromContext{}.AuthorizeMaster(r.Context())
		if err != nil {
			t.Fatalf("AuthorizeMaster err = %v", err)
		}
		if id == uuid.Nil {
			t.Fatal("master id should be non-zero on a successful auth")
		}
		if !mfa {
			t.Fatal("MFA flag should be true when MASTER_API_TOKEN set")
		}
		called = true
		w.WriteHeader(http.StatusOK)
	})
	wrapped := masterAuthMiddleware(getenv)(authed)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, r)
	if !called {
		t.Fatal("inner handler not invoked")
	}
}

func TestAuthorizeMaster_UnstampedContext_ReturnsError(t *testing.T) {
	t.Parallel()
	id, mfa, err := masterAuthorizerFromContext{}.AuthorizeMaster(context.Background())
	if err == nil {
		t.Fatal("expected error on unstamped context")
	}
	if id != uuid.Nil {
		t.Errorf("id = %s, want zero", id)
	}
	if mfa {
		t.Error("mfa should be false")
	}
}

func TestMasterAuthMiddleware_MissingBearerPrefix_Returns401(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == envMasterAPIToken {
			return "secret"
		}
		return ""
	}
	wrapped := masterAuthMiddleware(getenv)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("inner handler must not run")
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Basic abc")
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestSlogMasterAudit_LogsWithoutError(t *testing.T) {
	t.Parallel()
	a := slogMasterAudit{logger: slog.Default()}
	if err := a.LogMasterOverride(context.Background(), slugreservation.MasterOverrideEvent{
		Slug:     "demo",
		MasterID: uuid.New(),
		Reason:   "qa",
	}); err != nil {
		t.Fatalf("LogMasterOverride: %v", err)
	}
}

func TestFormSlugExtractor_NoBody_ReturnsFalse(t *testing.T) {
	t.Parallel()
	ext := formSlugExtractor("slug")
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if v, ok := ext(r); ok {
		t.Fatalf("ext returned (%q,%v), want (\"\",false)", v, ok)
	}
}

func TestRedirectHandler_NestedSubdomain_PassesThrough(t *testing.T) {
	t.Parallel()
	svc := mustService(t, &memSlugRedirectStore{m: map[string]string{"old": "new"}})
	called := false
	h := slugreservation.NewRedirectHandler(svc, "exemplo.com", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "deep.old.exemplo.com" // nested label, should not redirect.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if !called {
		t.Fatal("nested host must pass through to next")
	}
}
