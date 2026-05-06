package slugreservation_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/slugreservation"
)

type fakeAuth struct {
	masterID uuid.UUID
	mfa      bool
	err      error
}

func (a fakeAuth) AuthorizeMaster(_ context.Context) (uuid.UUID, bool, error) {
	return a.masterID, a.mfa, a.err
}

func TestOverrideHandler_HappyPath(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	svc, store, _, audit, slack := newSvc(t, now)
	store.rows["acme"] = slugreservation.Reservation{Slug: "acme", ExpiresAt: now.Add(time.Hour)}

	masterID := uuid.New()
	h := slugreservation.NewOverrideHandler(svc, fakeAuth{masterID: masterID, mfa: true}, true)
	mux := http.NewServeMux()
	h.Register(mux)

	body := strings.NewReader(`{"reason":"incident #42"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/master/slug-reservations/acme/release", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Slug      string `json:"slug"`
		ExpiresAt string `json:"expiresAt"`
		Status    string `json:"status"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Slug != "acme" || resp.Status != "released" {
		t.Fatalf("resp=%+v", resp)
	}
	if len(audit.calls) != 1 {
		t.Fatalf("audit calls=%d", len(audit.calls))
	}
	if len(slack.msgs) != 1 {
		t.Fatalf("slack msgs=%d", len(slack.msgs))
	}
}

func TestOverrideHandler_RequiresMaster(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSvc(t, time.Now())
	h := slugreservation.NewOverrideHandler(svc, fakeAuth{masterID: uuid.Nil}, true)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/master/slug-reservations/acme/release", strings.NewReader(`{"reason":"x"}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestOverrideHandler_AuthError(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSvc(t, time.Now())
	h := slugreservation.NewOverrideHandler(svc, fakeAuth{err: errors.New("expired")}, true)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/master/slug-reservations/acme/release", strings.NewReader(`{"reason":"x"}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestOverrideHandler_RequiresMFA(t *testing.T) {
	t.Parallel()
	now := time.Now()
	svc, store, _, _, _ := newSvc(t, now)
	store.rows["acme"] = slugreservation.Reservation{Slug: "acme", ExpiresAt: now.Add(time.Hour)}
	masterID := uuid.New()
	h := slugreservation.NewOverrideHandler(svc, fakeAuth{masterID: masterID, mfa: false}, true)

	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/master/slug-reservations/acme/release", strings.NewReader(`{"reason":"x"}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "mfa") {
		t.Fatalf("body=%q", rec.Body.String())
	}
}

func TestOverrideHandler_MFAOptional(t *testing.T) {
	t.Parallel()
	now := time.Now()
	svc, store, _, _, _ := newSvc(t, now)
	store.rows["acme"] = slugreservation.Reservation{Slug: "acme", ExpiresAt: now.Add(time.Hour)}
	masterID := uuid.New()
	h := slugreservation.NewOverrideHandler(svc, fakeAuth{masterID: masterID, mfa: false}, false)

	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/master/slug-reservations/acme/release", strings.NewReader(`{"reason":"x"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestOverrideHandler_BadJSON(t *testing.T) {
	t.Parallel()
	now := time.Now()
	svc, store, _, _, _ := newSvc(t, now)
	store.rows["acme"] = slugreservation.Reservation{Slug: "acme", ExpiresAt: now.Add(time.Hour)}
	h := slugreservation.NewOverrideHandler(svc, fakeAuth{masterID: uuid.New(), mfa: true}, false)

	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/master/slug-reservations/acme/release", strings.NewReader(`{not-json`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestOverrideHandler_ReasonMissing(t *testing.T) {
	t.Parallel()
	now := time.Now()
	svc, store, _, _, _ := newSvc(t, now)
	store.rows["acme"] = slugreservation.Reservation{Slug: "acme", ExpiresAt: now.Add(time.Hour)}
	h := slugreservation.NewOverrideHandler(svc, fakeAuth{masterID: uuid.New(), mfa: true}, false)

	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/master/slug-reservations/acme/release", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestOverrideHandler_NotReserved(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSvc(t, time.Now())
	h := slugreservation.NewOverrideHandler(svc, fakeAuth{masterID: uuid.New(), mfa: true}, false)

	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/master/slug-reservations/acme/release", strings.NewReader(`{"reason":"x"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestOverrideHandler_InvalidSlug(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSvc(t, time.Now())
	h := slugreservation.NewOverrideHandler(svc, fakeAuth{masterID: uuid.New(), mfa: true}, false)

	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/master/slug-reservations/BAD%21/release", strings.NewReader(`{"reason":"x"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestOverrideHandler_BodyTooLarge(t *testing.T) {
	t.Parallel()
	now := time.Now()
	svc, store, _, _, _ := newSvc(t, now)
	store.rows["acme"] = slugreservation.Reservation{Slug: "acme", ExpiresAt: now.Add(time.Hour)}
	h := slugreservation.NewOverrideHandler(svc, fakeAuth{masterID: uuid.New(), mfa: true}, false)

	huge := strings.Repeat("a", slugreservation.MaxOverrideBodyBytes+512)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/master/slug-reservations/acme/release", strings.NewReader(`{"reason":"`+huge+`"}`)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d", rec.Code)
	}
}
