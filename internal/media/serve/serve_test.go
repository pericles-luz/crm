package serve_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/media/serve"
	"github.com/pericles-luz/crm/internal/media/upload"
)

// memStore is the documented in-memory adapter the AGENTS.md quality
// bar permits in place of mocks: it implements the Store contract
// faithfully (including the path-enforcement contract from SIN-62226 §7)
// rather than recording calls.
type memStore struct {
	mu      sync.RWMutex
	logos   map[uuid.UUID]serve.Media
	byHash  map[string]serve.Media // keyed by tenantID|hash to enforce tenant scoping
	hardErr error                  // returned by both lookups when set
}

func newMemStore() *memStore {
	return &memStore{
		logos:  make(map[uuid.UUID]serve.Media),
		byHash: make(map[string]serve.Media),
	}
}

func storeKey(t uuid.UUID, h string) string { return t.String() + "|" + h }

func (s *memStore) putLogo(m serve.Media) { s.mu.Lock(); s.logos[m.TenantID] = m; s.mu.Unlock() }
func (s *memStore) putMedia(m serve.Media) {
	s.mu.Lock()
	s.byHash[storeKey(m.TenantID, m.Hash)] = m
	s.mu.Unlock()
}

func (s *memStore) LookupLogo(_ context.Context, tenantID uuid.UUID) (serve.Media, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.hardErr != nil {
		return serve.Media{}, s.hardErr
	}
	m, ok := s.logos[tenantID]
	if !ok {
		return serve.Media{}, serve.ErrNotFound
	}
	return m, nil
}

func (s *memStore) LookupHash(_ context.Context, tenantID uuid.UUID, hash string) (serve.Media, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.hardErr != nil {
		return serve.Media{}, s.hardErr
	}
	// Path enforcement: the hash must belong to this tenant. We do NOT
	// fall back to a global index even though one would be valid for
	// content-addressing — that is the SIN-62226 §7 guarantee under
	// test.
	m, ok := s.byHash[storeKey(tenantID, hash)]
	if !ok {
		return serve.Media{}, serve.ErrNotFound
	}
	return m, nil
}

// memBlob is a Blob backed by an in-memory `path → bytes` map.
type memBlob struct {
	mu       sync.RWMutex
	contents map[string][]byte
	openErr  error
}

func newMemBlob() *memBlob { return &memBlob{contents: map[string][]byte{}} }

func (b *memBlob) put(path string, data []byte) {
	b.mu.Lock()
	b.contents[path] = data
	b.mu.Unlock()
}

func (b *memBlob) Open(_ context.Context, path string) (io.ReadCloser, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.openErr != nil {
		return nil, b.openErr
	}
	data, ok := b.contents[path]
	if !ok {
		return nil, serve.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// errReadCloser is for the streaming-error path in TestServe_StreamingErrorIsSilent.
type errReadCloser struct{ closed bool }

func (e *errReadCloser) Read([]byte) (int, error) { return 0, errors.New("boom") }
func (e *errReadCloser) Close() error             { e.closed = true; return nil }

type errBlob struct{ rc *errReadCloser }

func (b *errBlob) Open(_ context.Context, _ string) (io.ReadCloser, error) {
	b.rc = &errReadCloser{}
	return b.rc, nil
}

func tenantA() uuid.UUID { return uuid.MustParse("00000000-0000-0000-0000-0000000000aa") }
func tenantB() uuid.UUID { return uuid.MustParse("00000000-0000-0000-0000-0000000000bb") }

const validHash = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func newServer(t *testing.T, store serve.Store, blob serve.Blob) *httptest.Server {
	t.Helper()
	h, err := serve.New(store, blob)
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}
	srv := httptest.NewServer(h.Routes())
	// Use Cleanup (not defer) so the server outlives parallel subtests
	// of the calling test.
	t.Cleanup(srv.Close)
	return srv
}

func TestNew_RejectsNilDeps(t *testing.T) {
	t.Parallel()
	if _, err := serve.New(nil, newMemBlob()); err == nil {
		t.Fatal("New(nil store) returned nil error")
	}
	if _, err := serve.New(newMemStore(), nil); err == nil {
		t.Fatal("New(nil blob) returned nil error")
	}
}

func TestServe_LogoOK(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	blob := newMemBlob()

	store.putLogo(serve.Media{
		TenantID:    tenantA(),
		Format:      upload.FormatPNG,
		StoragePath: "media/a/logo.png",
		SizeBytes:   3,
	})
	blob.put("media/a/logo.png", []byte("png"))

	srv := newServer(t, store, blob)

	res, err := http.Get(srv.URL + "/t/" + tenantA().String() + "/logo")
	if err != nil {
		t.Fatalf("GET logo: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", got)
	}
	if got := res.Header.Get("Cache-Control"); got != "private, max-age=300, must-revalidate" {
		t.Fatalf("logo Cache-Control = %q", got)
	}
	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff missing, got %q", got)
	}
	if got := res.Header.Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") || !strings.Contains(got, "img-src 'self'") {
		t.Fatalf("CSP = %q", got)
	}
	if got := res.Header.Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q", got)
	}
	if cd := res.Header.Get("Content-Disposition"); cd != "" {
		t.Fatalf("Content-Disposition should be empty for image, got %q", cd)
	}
	if cl := res.Header.Get("Content-Length"); cl != "3" {
		t.Fatalf("Content-Length = %q, want 3", cl)
	}

	got, _ := io.ReadAll(res.Body)
	if string(got) != "png" {
		t.Fatalf("body = %q, want png", string(got))
	}
}

func TestServe_LogoNotFound(t *testing.T) {
	t.Parallel()
	srv := newServer(t, newMemStore(), newMemBlob())

	res, err := http.Get(srv.URL + "/t/" + tenantA().String() + "/logo")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}

func TestServe_LogoStoreError_500(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	store.hardErr = errors.New("db down")

	srv := newServer(t, store, newMemBlob())

	res, err := http.Get(srv.URL + "/t/" + tenantA().String() + "/logo")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
}

func TestServe_LogoBlobMissing_404(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	store.putLogo(serve.Media{
		TenantID:    tenantA(),
		Format:      upload.FormatPNG,
		StoragePath: "media/a/missing.png",
	})
	srv := newServer(t, store, newMemBlob())

	res, err := http.Get(srv.URL + "/t/" + tenantA().String() + "/logo")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when blob is missing", res.StatusCode)
	}
}

func TestServe_LogoBlobError_500(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	store.putLogo(serve.Media{
		TenantID:    tenantA(),
		Format:      upload.FormatPNG,
		StoragePath: "media/a/logo.png",
	})
	blob := newMemBlob()
	blob.openErr = errors.New("disk full")

	srv := newServer(t, store, blob)

	res, err := http.Get(srv.URL + "/t/" + tenantA().String() + "/logo")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
}

func TestServe_LogoUnknownFormat_500(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	store.putLogo(serve.Media{
		TenantID:    tenantA(),
		Format:      upload.Format("bmp"),
		StoragePath: "media/a/logo.bmp",
	})
	srv := newServer(t, store, newMemBlob())

	res, err := http.Get(srv.URL + "/t/" + tenantA().String() + "/logo")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
}

func TestServe_LogoBadTenantUUID_404(t *testing.T) {
	t.Parallel()
	srv := newServer(t, newMemStore(), newMemBlob())

	cases := []string{"not-a-uuid", "00000000-0000-0000-0000-000000000000"} // nil UUID also rejected
	for _, c := range cases {
		c := c
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			res, err := http.Get(srv.URL + "/t/" + c + "/logo")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 for tenant %q", res.StatusCode, c)
			}
		})
	}
}

func TestServe_ContentOK_PerFormatMime(t *testing.T) {
	t.Parallel()
	// Exercise every accepted Format so mimeFor stays exercised end-to-end.
	store := newMemStore()
	blob := newMemBlob()

	type entry struct {
		fmt     upload.Format
		hash    string
		path    string
		want    string
		isImage bool
	}
	entries := []entry{
		{upload.FormatJPEG, "11" + validHash[2:], "media/a/jpeg.jpg", "image/jpeg", true},
		{upload.FormatWEBP, "22" + validHash[2:], "media/a/webp.webp", "image/webp", true},
	}
	for _, e := range entries {
		store.putMedia(serve.Media{
			TenantID:    tenantA(),
			Hash:        e.hash,
			Format:      e.fmt,
			StoragePath: e.path,
			SizeBytes:   1,
		})
		blob.put(e.path, []byte("x"))
	}

	srv := newServer(t, store, blob)
	for _, e := range entries {
		e := e
		t.Run(string(e.fmt), func(t *testing.T) {
			t.Parallel()
			res, err := http.Get(srv.URL + "/t/" + tenantA().String() + "/m/" + e.hash)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", res.StatusCode)
			}
			if got := res.Header.Get("Content-Type"); got != e.want {
				t.Fatalf("Content-Type = %q, want %q", got, e.want)
			}
			if cd := res.Header.Get("Content-Disposition"); cd != "" {
				t.Fatalf("image Content-Disposition = %q, want empty", cd)
			}
		})
	}
}

func TestServe_ContentOK_PNG(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	blob := newMemBlob()

	store.putMedia(serve.Media{
		TenantID:    tenantA(),
		Hash:        validHash,
		Format:      upload.FormatPNG,
		StoragePath: "media/a/" + validHash + ".png",
		SizeBytes:   5,
	})
	blob.put("media/a/"+validHash+".png", []byte("hello"))

	srv := newServer(t, store, blob)

	res, err := http.Get(srv.URL + "/t/" + tenantA().String() + "/m/" + validHash)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Cache-Control"); got != "private, max-age=31536000, immutable" {
		t.Fatalf("immutable Cache-Control = %q", got)
	}
	if got := res.Header.Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q", got)
	}
	if cd := res.Header.Get("Content-Disposition"); cd != "" {
		t.Fatalf("image content disposition should be empty, got %q", cd)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != "hello" {
		t.Fatalf("body = %q", string(body))
	}
}

func TestServe_ContentOK_PDF_AttachmentDisposition(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	blob := newMemBlob()

	store.putMedia(serve.Media{
		TenantID:    tenantA(),
		Hash:        validHash,
		Format:      upload.FormatPDF,
		StoragePath: "media/a/" + validHash + ".pdf",
		SizeBytes:   3,
		Filename:    "Relatório Final\r\n\".pdf",
	})
	blob.put("media/a/"+validHash+".pdf", []byte("PDF"))

	srv := newServer(t, store, blob)

	res, err := http.Get(srv.URL + "/t/" + tenantA().String() + "/m/" + validHash)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/pdf" {
		t.Fatalf("Content-Type = %q", ct)
	}
	cd := res.Header.Get("Content-Disposition")
	if !strings.HasPrefix(cd, `attachment; filename="`) || !strings.HasSuffix(cd, `"`) {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	// Sanitization MUST kill CR, LF, quote, backslash, and non-ASCII so
	// no header smuggling and no UTF-8 confusion.
	for _, banned := range []string{"\r", "\n", `"`, `\`, "ó"} {
		if strings.Contains(cd[len("attachment; filename=\""):len(cd)-1], banned) {
			t.Fatalf("Content-Disposition leaked banned char %q in %q", banned, cd)
		}
	}
}

func TestServe_ContentEmptyFilename_FallsBackToFile(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	blob := newMemBlob()

	store.putMedia(serve.Media{
		TenantID:    tenantA(),
		Hash:        validHash,
		Format:      upload.FormatPDF,
		StoragePath: "media/a/" + validHash + ".pdf",
		Filename:    "   \r\n  ",
	})
	blob.put("media/a/"+validHash+".pdf", []byte("PDF"))

	srv := newServer(t, store, blob)

	res, err := http.Get(srv.URL + "/t/" + tenantA().String() + "/m/" + validHash)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()

	if cd := res.Header.Get("Content-Disposition"); cd != `attachment; filename="file"` {
		t.Fatalf("Content-Disposition = %q, want fallback to file", cd)
	}
}

func TestServe_ContentCrossTenantIsolation_404(t *testing.T) {
	t.Parallel()
	// Hash exists for tenant A but the request asks for it under tenant B.
	store := newMemStore()
	blob := newMemBlob()

	store.putMedia(serve.Media{
		TenantID:    tenantA(),
		Hash:        validHash,
		Format:      upload.FormatPNG,
		StoragePath: "media/a/" + validHash + ".png",
	})
	blob.put("media/a/"+validHash+".png", []byte("X"))

	srv := newServer(t, store, blob)

	res, err := http.Get(srv.URL + "/t/" + tenantB().String() + "/m/" + validHash)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()

	// SIN-62226 §7: 404 (NOT 403) so existence cross-tenant does not leak.
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (cross-tenant must not leak)", res.StatusCode)
	}
}

func TestServe_ContentInvalidHash_404(t *testing.T) {
	t.Parallel()
	srv := newServer(t, newMemStore(), newMemBlob())

	cases := []string{
		"short",
		strings.Repeat("g", 64), // not hex
		strings.Repeat("A", 64), // uppercase not allowed
	}
	for _, h := range cases {
		h := h
		t.Run(h, func(t *testing.T) {
			t.Parallel()
			res, err := http.Get(srv.URL + "/t/" + tenantA().String() + "/m/" + h)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 for hash %q", res.StatusCode, h)
			}
		})
	}
}

func TestServe_ContentStoreError_500(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	store.hardErr = errors.New("db down")
	srv := newServer(t, store, newMemBlob())

	res, err := http.Get(srv.URL + "/t/" + tenantA().String() + "/m/" + validHash)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
}

func TestServe_ContentBlobOpenError_500(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	store.putMedia(serve.Media{
		TenantID:    tenantA(),
		Hash:        validHash,
		Format:      upload.FormatPNG,
		StoragePath: "media/a/" + validHash + ".png",
	})
	blob := newMemBlob()
	blob.openErr = errors.New("disk full")

	srv := newServer(t, store, blob)

	res, err := http.Get(srv.URL + "/t/" + tenantA().String() + "/m/" + validHash)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
}

func TestServe_ContentBadTenantUUID_404(t *testing.T) {
	t.Parallel()
	srv := newServer(t, newMemStore(), newMemBlob())

	res, err := http.Get(srv.URL + "/t/not-a-uuid/m/" + validHash)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}

func TestServe_NonGETOnLogo_405(t *testing.T) {
	t.Parallel()
	srv := newServer(t, newMemStore(), newMemBlob())

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/t/"+tenantA().String()+"/logo", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", res.StatusCode)
	}
}

func TestServe_StreamingErrorIsSilent(t *testing.T) {
	t.Parallel()
	// The blob opens fine but reads always fail. Headers will already
	// be on the wire when io.Copy fails; the handler must not panic and
	// must not retry / write a second status code.
	store := newMemStore()
	store.putMedia(serve.Media{
		TenantID:    tenantA(),
		Hash:        validHash,
		Format:      upload.FormatPNG,
		StoragePath: "ok",
		SizeBytes:   10,
	})
	blob := &errBlob{}

	srv := newServer(t, store, blob)

	res, err := http.Get(srv.URL + "/t/" + tenantA().String() + "/m/" + validHash)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()

	// Headers already committed → status 200; streaming error is
	// surfaced as a body-read failure on the client, not a panic.
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	_, _ = io.ReadAll(res.Body) // best-effort
	if blob.rc == nil || !blob.rc.closed {
		t.Fatalf("blob ReadCloser was not closed (rc=%+v)", blob.rc)
	}
}

func TestServe_NeverEmitsSetCookie(t *testing.T) {
	t.Parallel()
	// Static origin must not leak cookies even on error responses.
	// Caddy strips Set-Cookie outbound, but we double-check at the Go
	// layer that nothing in this package ever sets one.
	store := newMemStore()
	blob := newMemBlob()

	store.putLogo(serve.Media{
		TenantID:    tenantA(),
		Format:      upload.FormatPNG,
		StoragePath: "media/a/logo.png",
		SizeBytes:   1,
	})
	blob.put("media/a/logo.png", []byte("x"))

	srv := newServer(t, store, blob)

	cases := []string{
		"/t/" + tenantA().String() + "/logo",           // 200
		"/t/" + tenantB().String() + "/logo",           // 404
		"/t/" + tenantA().String() + "/m/" + validHash, // 404 (no media)
		"/t/not-a-uuid/m/" + validHash,                 // 404
		"/t/" + tenantA().String() + "/m/short",        // 404
	}
	for _, p := range cases {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+p, nil)
		req.Header.Set("Cookie", "__Host-sess=should-not-leak")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		if got := res.Header.Values("Set-Cookie"); len(got) != 0 {
			t.Fatalf("path %s emitted Set-Cookie: %v", p, got)
		}
		res.Body.Close()
	}
}

func TestServe_ContentLengthZeroOmitted(t *testing.T) {
	t.Parallel()
	// SizeBytes=0 means "store didn't populate it"; we must not write a
	// bogus Content-Length: 0 header that would conflict with the real
	// streamed body length.
	store := newMemStore()
	blob := newMemBlob()
	store.putLogo(serve.Media{
		TenantID:    tenantA(),
		Format:      upload.FormatPNG,
		StoragePath: "media/a/logo.png",
		SizeBytes:   0,
	})
	blob.put("media/a/logo.png", bytes.Repeat([]byte("z"), 17))
	srv := newServer(t, store, blob)

	res, err := http.Get(srv.URL + "/t/" + tenantA().String() + "/logo")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()

	cl := res.Header.Get("Content-Length")
	if cl == "" {
		// http.ResponseWriter may auto-add a length header from the body
		// — accept that as long as it matches reality.
	} else {
		n, err := strconv.Atoi(cl)
		if err != nil || n != 17 {
			t.Fatalf("Content-Length = %q (parsed %d, err %v), want either empty or 17", cl, n, err)
		}
	}
	body, _ := io.ReadAll(res.Body)
	if len(body) != 17 {
		t.Fatalf("body length = %d, want 17", len(body))
	}
}

// TestServe_CORPSameOriginEverywhere is the SIN-62330 regression: every
// response from the static origin — 200 and 404, logo and content-addressed
// — must carry Cross-Origin-Resource-Policy: same-origin so a cross-origin
// embedder cannot hot-link tenant assets and read pixel data via canvas.
func TestServe_CORPSameOriginEverywhere(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	blob := newMemBlob()

	store.putLogo(serve.Media{
		TenantID:    tenantA(),
		Format:      upload.FormatPNG,
		StoragePath: "media/a/logo.png",
		SizeBytes:   1,
	})
	blob.put("media/a/logo.png", []byte("x"))

	store.putMedia(serve.Media{
		TenantID:    tenantA(),
		Hash:        validHash,
		Format:      upload.FormatPNG,
		StoragePath: "media/a/" + validHash + ".png",
		SizeBytes:   1,
	})
	blob.put("media/a/"+validHash+".png", []byte("y"))

	srv := newServer(t, store, blob)

	cases := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{"200 logo", "/t/" + tenantA().String() + "/logo", http.StatusOK},
		{"200 content-addressed", "/t/" + tenantA().String() + "/m/" + validHash, http.StatusOK},
		{"404 logo", "/t/" + tenantB().String() + "/logo", http.StatusNotFound},
		{"404 cross-tenant", "/t/" + tenantB().String() + "/m/" + validHash, http.StatusNotFound},
		{"404 invalid hash", "/t/" + tenantA().String() + "/m/short", http.StatusNotFound},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			res, err := http.Get(srv.URL + c.path)
			if err != nil {
				t.Fatalf("GET %s: %v", c.path, err)
			}
			defer res.Body.Close()
			if res.StatusCode != c.wantStatus {
				t.Fatalf("status = %d, want %d", res.StatusCode, c.wantStatus)
			}
			if got := res.Header.Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
				t.Fatalf("CORP = %q, want same-origin", got)
			}
		})
	}
}
