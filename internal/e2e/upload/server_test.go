//go:build e2e

package upload_test

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	uploadweb "github.com/pericles-luz/crm/internal/adapter/web/upload"
)

// testdataFS embeds the test-only assets (vendored htmx) — kept out of
// the production embedded FS so the htmx blob can never leak via the
// public /static/upload/ handler.
//
//go:embed testdata/htmx.min.js
var testdataFS embed.FS

const pageHTML = `<!doctype html>
<html lang="pt-br">
<head>
<meta charset="utf-8">
<title>upload e2e — {{.Kind}}</title>
<script src="/test-static/htmx.min.js"></script>
<link rel="stylesheet" href="/static/upload/upload.css">
</head>
<body>
<header><h1>Upload de {{.Kind}}</h1></header>
<main id="form-host">
{{.Form}}
</main>
<script src="/static/upload/upload.js"></script>
</body>
</html>
`

// postCounter wraps a per-test counter so scenarios can assert that no
// XHR was fired (scenarios 1 and 3) or that exactly one was (scenario 2).
type postCounter struct {
	count int64
}

func (c *postCounter) inc() { atomic.AddInt64(&c.count, 1) }
func (c *postCounter) get() int64 {
	return atomic.LoadInt64(&c.count)
}

// postBehaviour is what the per-test POST handler does when the upload
// form posts. The counter is incremented before the handler runs.
type postBehaviour func(w http.ResponseWriter, r *http.Request)

// postOK swaps in a small success snippet. HTMX replaces #form-host with it.
func postOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<p data-upload-success>Logo enviado com sucesso.</p>`))
}

// post415 returns an unsupported-media-type response with a code body the
// upload.js translator recognises as a status (no "code" override).
func post415(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnsupportedMediaType)
	_, _ = w.Write([]byte(`{"error":"unsupported"}`))
}

// postBoom is the safety-net for scenarios where the test expects ZERO
// XHRs. If the client *does* send one, we want a loud failure rather
// than a silent 200, so the test can blame the boundary clearly.
func postBoom(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "client should not have POSTed", http.StatusTeapot)
}

// startServer wires the upload form's GET handler, a configurable POST
// handler, the embedded production static FS, and the test-only htmx
// asset under separate URL prefixes. Returns the test server and a
// counter the caller can poll for "did we receive a POST?".
func startServer(t *testing.T, post postBehaviour) (*httptest.Server, *postCounter) {
	t.Helper()
	if post == nil {
		post = postBoom
	}
	tmpl, err := template.New("page").Parse(pageHTML)
	if err != nil {
		t.Fatalf("page template: %v", err)
	}
	staticFS, err := fs.Sub(testdataFS, "testdata")
	if err != nil {
		t.Fatalf("test-static sub: %v", err)
	}

	counter := &postCounter{}

	mux := http.NewServeMux()
	mux.HandleFunc("/uploads/logo", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			renderPage(t, tmpl, w, uploadweb.KindLogo)
		case http.MethodPost:
			counter.inc()
			post(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.Handle("/static/upload/", http.StripPrefix("/static/upload/", uploadweb.StaticHandler()))
	mux.Handle("/test-static/", http.StripPrefix("/test-static/", http.FileServerFS(staticFS)))

	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s, counter
}

func renderPage(t *testing.T, tmpl *template.Template, w http.ResponseWriter, kind uploadweb.Kind) {
	t.Helper()
	var form strings.Builder
	if err := uploadweb.Render(&form, kind, uploadweb.FormConfig{
		Action: "/uploads/logo",
		Target: "#form-host",
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := tmpl.Execute(w, struct {
		Kind string
		Form template.HTML
	}{
		Kind: string(kind),
		Form: template.HTML(form.String()),
	}); err != nil {
		t.Logf("render page: %v", err)
	}
}

// fixturePath resolves a fixture filename to an absolute disk path that
// chromedp's SetUploadFiles can pass to Chrome. The fixtures live under
// internal/adapter/web/upload/static/testdata/.
func fixturePath(t *testing.T, name string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile is .../internal/e2e/upload/server_test.go — climb to repo
	// root, then descend into the production testdata dir.
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	path := filepath.Join(repoRoot, "internal", "adapter", "web", "upload", "static", "testdata", name)
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs(%s): %v", path, err)
	}
	return abs
}

// formPageURL is the canonical entry point each scenario navigates to.
func formPageURL(s *httptest.Server) string {
	return fmt.Sprintf("%s/uploads/logo", s.URL)
}
