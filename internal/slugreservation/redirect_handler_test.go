package slugreservation_test

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/slugreservation"
)

func TestRedirectHandler_Redirects(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	svc, _, red, _, _ := newSvc(t, now)
	red.rows["acme"] = slugreservation.Redirect{OldSlug: "acme", NewSlug: "acme-2", ExpiresAt: now.Add(time.Hour)}
	h := slugreservation.NewRedirectHandler(svc, "crm.example.test", nil)

	r := httptest.NewRequest(http.MethodGet, "/dashboard?x=1", nil)
	r.Host = "acme.crm.example.test"
	r.TLS = &tls.ConnectionState{}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("code=%d", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "https://acme-2.crm.example.test/dashboard?x=1" {
		t.Fatalf("location=%q", loc)
	}
	if w.Header().Get("Clear-Site-Data") != `"cookies"` {
		t.Fatalf("clear-site-data=%q", w.Header().Get("Clear-Site-Data"))
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control=%q", w.Header().Get("Cache-Control"))
	}
}

func TestRedirectHandler_FallsThrough(t *testing.T) {
	t.Parallel()
	cases := []string{
		"acme.crm.example.test", // no redirect record
		"crm.example.test",      // primary host itself
		"acme.bar.example.test", // wrong primary
		"a.b.crm.example.test",  // nested label not allowed
		"",                      // empty host
	}
	for _, host := range cases {
		host := host
		t.Run(host, func(t *testing.T) {
			t.Parallel()
			svc, _, _, _, _ := newSvc(t, time.Now())
			var called bool
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})
			h := slugreservation.NewRedirectHandler(svc, "crm.example.test", next)
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Host = host
			h.ServeHTTP(httptest.NewRecorder(), r)
			if !called {
				t.Fatalf("next not called for host %q", host)
			}
		})
	}
}

func TestRedirectHandler_NoNext_DefaultsTo404(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSvc(t, time.Now())
	h := slugreservation.NewRedirectHandler(svc, "crm.example.test", nil)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "acme.crm.example.test" // no redirect rule → falls through to default 404
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestRedirectHandler_PrimaryEmpty_FallsThrough(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSvc(t, time.Now())
	called := false
	h := slugreservation.NewRedirectHandler(svc, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "acme.crm.example.test"
	h.ServeHTTP(httptest.NewRecorder(), r)
	if !called {
		t.Fatal("next not called")
	}
}

func TestRedirectHandler_StripsPort(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	svc, _, red, _, _ := newSvc(t, now)
	red.rows["acme"] = slugreservation.Redirect{OldSlug: "acme", NewSlug: "acme-2", ExpiresAt: now.Add(time.Hour)}
	h := slugreservation.NewRedirectHandler(svc, "crm.example.test", nil)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "acme.crm.example.test:8443"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestRedirectHandler_HonoursForwardedProto(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	svc, _, red, _, _ := newSvc(t, now)
	red.rows["acme"] = slugreservation.Redirect{OldSlug: "acme", NewSlug: "acme-2", ExpiresAt: now.Add(time.Hour)}
	h := slugreservation.NewRedirectHandler(svc, "crm.example.test", nil)

	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Host = "acme.crm.example.test"
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got := w.Header().Get("Location"); got != "https://acme-2.crm.example.test/x" {
		t.Fatalf("location=%q", got)
	}
}

func TestRedirectHandler_PlaintextDev(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	svc, _, red, _, _ := newSvc(t, now)
	red.rows["acme"] = slugreservation.Redirect{OldSlug: "acme", NewSlug: "acme-2", ExpiresAt: now.Add(time.Hour)}
	h := slugreservation.NewRedirectHandler(svc, "crm.example.test", nil)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "acme.crm.example.test"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got := w.Header().Get("Location"); got != "http://acme-2.crm.example.test/" {
		t.Fatalf("location=%q", got)
	}
}
