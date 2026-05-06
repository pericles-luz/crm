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

	"github.com/pericles-luz/crm/internal/slugreservation"
)

func TestRequireSlugAvailable_Pass(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSvc(t, time.Now())
	mux := http.NewServeMux()
	called := false
	mw := slugreservation.RequireSlugAvailable(svc, slugreservation.PathValueExtractor("slug"))
	mux.Handle("POST /tenants/{slug}", mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	})))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tenants/acme", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d", rec.Code)
	}
	if !called {
		t.Fatal("next not called")
	}
}

func TestRequireSlugAvailable_Conflict(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	svc, store, _, _, _ := newSvc(t, now)
	store.rows["acme"] = slugreservation.Reservation{
		Slug:      "acme",
		ExpiresAt: now.Add(slugreservation.ReservationWindow),
	}
	mux := http.NewServeMux()
	mw := slugreservation.RequireSlugAvailable(svc, slugreservation.PathValueExtractor("slug"))
	mux.Handle("POST /tenants/{slug}", mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next must not run")
		w.WriteHeader(http.StatusOK)
	})))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tenants/acme", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d", rec.Code)
	}
	var body struct {
		Slug          string `json:"slug"`
		ReservedUntil string `json:"reservedUntil"`
		Reason        string `json:"reason"`
		Message       string `json:"message"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Slug != "acme" || body.Reason != "reserved" {
		t.Fatalf("body=%+v", body)
	}
	if !strings.HasPrefix(body.Message, "this slug is reserved until ") {
		t.Fatalf("message=%q", body.Message)
	}
	if body.ReservedUntil == "" {
		t.Fatal("missing reservedUntil")
	}
}

func TestRequireSlugAvailable_BadInput(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSvc(t, time.Now())
	mw := slugreservation.RequireSlugAvailable(svc, func(_ *http.Request) (string, bool) {
		return "BAD!", true
	})
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("must not run")
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestRequireSlugAvailable_NoSlug(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSvc(t, time.Now())
	mw := slugreservation.RequireSlugAvailable(svc, func(_ *http.Request) (string, bool) {
		return "", false
	})
	called := false
	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	if !called {
		t.Fatal("next not called")
	}
}

type errSvcStore struct{ slugreservation.Store }

func (errSvcStore) Active(context.Context, string) (slugreservation.Reservation, error) {
	return slugreservation.Reservation{}, errors.New("boom")
}

func TestRequireSlugAvailable_StoreError(t *testing.T) {
	t.Parallel()
	clk := fixedClock{t: time.Now()}
	red := newFakeRedirectStore(clk)
	audit := &fakeAudit{}
	slack := &fakeSlack{}
	svc, err := slugreservation.NewService(errSvcStore{}, red, audit, slack, clk)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	mw := slugreservation.RequireSlugAvailable(svc, slugreservation.PathValueExtractor("slug"))
	mux := http.NewServeMux()
	mux.Handle("POST /tenants/{slug}", mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("must not run")
	})))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tenants/acme", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestPathValueExtractor_Empty(t *testing.T) {
	t.Parallel()
	ex := slugreservation.PathValueExtractor("slug")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if v, ok := ex(r); ok || v != "" {
		t.Fatalf("got=%q ok=%v", v, ok)
	}
}
