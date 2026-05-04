package middleware

import (
	"context"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureNonce is a request handler that copies the per-request nonce from
// the context into a closure-owned slot so the test can compare it to the
// header value sent by the middleware.
func captureNonce(into *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*into = CSPNonce(r.Context())
		_, _ = io.WriteString(w, "ok")
	})
}

func TestCSPNonce_OutsideCtxIsEmpty(t *testing.T) {
	t.Parallel()
	var nilCtx context.Context //nolint:revive // intentional: CSPNonce must tolerate a nil interface so template helpers fail closed.
	if got := CSPNonce(nilCtx); got != "" {
		t.Fatalf("nil ctx must return empty nonce, got %q", got)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := CSPNonce(req.Context()); got != "" {
		t.Fatalf("uninstrumented ctx must return empty nonce, got %q", got)
	}
}

func TestCSP_ConsecutiveRequestsHaveDistinctNonces(t *testing.T) {
	t.Parallel()
	mw := CSP()

	var first, second string
	h1 := mw(captureNonce(&first))
	h2 := mw(captureNonce(&second))

	w1 := httptest.NewRecorder()
	h1.ServeHTTP(w1, httptest.NewRequest(http.MethodGet, "/", nil))
	w2 := httptest.NewRecorder()
	h2.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/", nil))

	if first == "" || second == "" {
		t.Fatalf("both nonces must be populated; got %q and %q", first, second)
	}
	if first == second {
		t.Fatalf("two consecutive requests must have distinct nonces; both = %q", first)
	}
	header1 := w1.Header().Get("Content-Security-Policy")
	header2 := w2.Header().Get("Content-Security-Policy")
	if !strings.Contains(header1, "'nonce-"+first+"'") {
		t.Fatalf("CSP header on req1 must contain nonce; header=%q nonce=%q", header1, first)
	}
	if !strings.Contains(header2, "'nonce-"+second+"'") {
		t.Fatalf("CSP header on req2 must contain nonce; header=%q nonce=%q", header2, second)
	}
}

func TestCSP_HeaderTemplateContainsRequiredDirectives(t *testing.T) {
	t.Parallel()
	mw := CSP()
	h := mw(captureNonce(new(string)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	header := w.Header().Get("Content-Security-Policy")
	for _, directive := range []string{
		"default-src 'self'",
		"script-src 'self' 'nonce-",
		"frame-ancestors 'none'",
		"object-src 'none'",
		"base-uri 'self'",
		"form-action 'self'",
		"upgrade-insecure-requests",
	} {
		if !strings.Contains(header, directive) {
			t.Fatalf("CSP header missing %q; got %q", directive, header)
		}
	}
	if strings.Contains(header, "{nonce}") {
		t.Fatalf("CSP header still contains literal {nonce}; got %q", header)
	}
}

func TestCSP_TemplateRendersScriptWithNonceMatchingHeader(t *testing.T) {
	t.Parallel()
	// An end-to-end test that mimics how a server-rendered page consumes
	// the nonce: the handler renders an html/template with a `cspNonce`
	// helper, and we verify the rendered <script nonce> matches the header
	// the middleware emitted on the same response.
	tmpl := template.Must(template.New("page").Funcs(template.FuncMap{
		"cspNonce": func() string { return "" }, // overridden per request below
	}).Parse(`<script nonce="{{ cspNonce }}">/* x */</script>`))

	mw := CSP()
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce := CSPNonce(r.Context())
		// Re-bind cspNonce per request so the helper returns the
		// request-scoped nonce.
		page := template.Must(tmpl.Clone())
		page.Funcs(template.FuncMap{"cspNonce": func() string { return nonce }})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = page.Execute(w, nil)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	header := rec.Header().Get("Content-Security-Policy")
	body := rec.Body.String()
	// The header carries 'nonce-<value>', the body carries nonce="<value>".
	const headerPrefix = "'nonce-"
	idx := strings.Index(header, headerPrefix)
	if idx < 0 {
		t.Fatalf("header missing nonce directive: %q", header)
	}
	rest := header[idx+len(headerPrefix):]
	end := strings.IndexByte(rest, '\'')
	if end < 0 {
		t.Fatalf("header missing nonce close quote: %q", header)
	}
	headerNonce := rest[:end]
	if !strings.Contains(body, `nonce="`+headerNonce+`"`) {
		t.Fatalf("body nonce does not match header nonce.\n header=%q\n body=%q", headerNonce, body)
	}
}

func TestCSP_NonceLengthMatchesSpec(t *testing.T) {
	t.Parallel()
	mw := CSP()
	var captured string
	h := mw(captureNonce(&captured))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	// 16 bytes → 22 base64url-no-pad chars.
	const want = 22
	if len(captured) != want {
		t.Fatalf("nonce length = %d, want %d (base64url of 16 bytes)", len(captured), want)
	}
}

func TestCSP_FailsClosedOnRandSourceError(t *testing.T) {
	t.Parallel()
	mw := cspWith(func([]byte) (int, error) { return 0, errors.New("entropy depleted") })
	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 on rand failure", rec.Code)
	}
	if called {
		t.Fatal("downstream handler must not run when nonce generation fails")
	}
}
