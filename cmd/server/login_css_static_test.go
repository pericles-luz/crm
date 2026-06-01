package main

// SIN-63941 / UX-F4 — regression guard for the redesigned /login
// stylesheet.
//
// internal/adapter/httpapi/views/layout.html references
// /static/css/login.css; if the file is missing on disk the link tag
// 404s silently and the /login surface renders with only the
// SIN-63294 baseline (auth.css), losing the branded card layout +
// tenant-logo header. Spinning up the same FileServer setup that
// production main.go mounts proves the asset exists and is served as
// text/css through the registered static handler — parallel to the
// SIN-63294 TestAuthStylesheet_ServedAsCSS in auth_css_static_test.go
// and the SIN-63935 design-system test in design_system_css_static_test.go.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoginStylesheet_ServedAsCSS(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/login.css", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/login.css must exist", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("served body is empty — login.css must have rules")
	}
	// Spot-check the load-bearing selectors. Each entry below gates a
	// distinct visual concern: outer page container, card surface,
	// branded header, wordmark fallback, token consumption (so a
	// future refactor that hard-codes a colour back to #1f6feb fails
	// here instead of in staging).
	for _, needle := range []string{
		".login-page",
		".login-card",
		".login-card__header",
		".login-card__wordmark",
		".login-card__title",
		"var(--space-",
		"var(--surface-",
		"var(--color-primary",
		"@media (max-width: 480px)",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("login.css missing required selector or token reference %q", needle)
		}
	}
}
