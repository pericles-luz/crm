package serve_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/media/serve"
)

func TestMediaHeaders_SetsBaselineOnInnerHandler(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := httptest.NewServer(serve.MediaHeaders(inner))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/anything")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()

	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff = %q", got)
	}
	if got := res.Header.Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q", got)
	}
	csp := res.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "img-src 'self'", "style-src 'unsafe-inline'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP %q missing %q", csp, want)
		}
	}
}

// TestMediaHeaders_SetsCORPSameOrigin is the SIN-62330 unit-level
// regression for the middleware itself: CORP same-origin must be applied
// on every response.
func TestMediaHeaders_SetsCORPSameOrigin(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(serve.MediaHeaders(inner))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/anything")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()

	if got := res.Header.Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
		t.Fatalf("CORP = %q, want same-origin", got)
	}
}

func TestMediaHeaders_HandlerCanOverrideCacheControl(t *testing.T) {
	t.Parallel()
	// Middleware must NOT clobber Cache-Control values set later by the
	// inner handler.
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "private, max-age=42")
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(serve.MediaHeaders(inner))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()

	if got := res.Header.Get("Cache-Control"); got != "private, max-age=42" {
		t.Fatalf("Cache-Control = %q", got)
	}
}
