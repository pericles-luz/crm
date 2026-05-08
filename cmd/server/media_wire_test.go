package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	mediaserve "github.com/pericles-luz/crm/internal/media/serve"
	mediaupload "github.com/pericles-luz/crm/internal/media/upload"
)

func TestMediaUpload_SVG_Returns415(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	registerUploadRoutes(mux)

	body, ct := multipartFile(t, "logo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	r := httptest.NewRequest(http.MethodPost, "/api/tenant/uploads/logo", body)
	r.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnsupportedMediaType)
	}
}

func TestMediaUpload_PNG_Returns200WithHash(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	registerUploadRoutes(mux)

	body, ct := multipartFile(t, "logo.png", makeTinyPNG(t))
	r := httptest.NewRequest(http.MethodPost, "/api/tenant/uploads/logo", body)
	r.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var out map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out["hash"]) != 64 {
		t.Fatalf("hash length = %d, want 64", len(out["hash"]))
	}
	if out["format"] != string(mediaupload.FormatPNG) {
		t.Fatalf("format = %q, want %q", out["format"], mediaupload.FormatPNG)
	}
}

func TestMediaUpload_MissingFile_Returns400(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	registerUploadRoutes(mux)

	// Empty multipart form, no `file` field at all.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("other", "value")
	mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/tenant/uploads/logo", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestMediaUpload_WrongMethod_Returns405(t *testing.T) {
	t.Parallel()
	// We mount the handler directly here because the mux is registered
	// with `POST /api/...` and would 405 itself before our handler ran;
	// covering the handler-level guard keeps the contract documented.
	r := httptest.NewRequest(http.MethodGet, "/api/tenant/uploads/logo", nil)
	rec := httptest.NewRecorder()
	mediaUploadHandler(logoUploadPolicy).ServeHTTP(rec, r)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestMediaServe_Logo_NotFound_StillSetsHeaders(t *testing.T) {
	t.Parallel()
	h, err := buildMediaServeHandler()
	if err != nil {
		t.Fatalf("buildMediaServeHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/t/"+uuid.New().String()+"/logo", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	// 404 from nopMediaStorage; headers must still be present so a real
	// adapter swap-in does not regress the static-origin posture.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertStaticOriginHeaders(t, rec.Header())
}

func TestMediaServe_Logo_OkPath_SetsHeaders(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	store := stubServeStore{
		logo: mediaserve.Media{
			TenantID:    tenantID,
			Hash:        strings.Repeat("a", 64),
			Format:      mediaupload.FormatPNG,
			StoragePath: "media/logo.png",
			SizeBytes:   8,
		},
	}
	h, err := mediaserve.New(store, stubServeBlob{payload: []byte("bytesxxx")})
	if err != nil {
		t.Fatalf("mediaserve.New: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/t/"+tenantID.String()+"/logo", nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", got)
	}
	assertStaticOriginHeaders(t, rec.Header())
}

func TestRunStaticOrigin_ShutsDownOnContextCancel(t *testing.T) {
	t.Parallel()
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	handler, err := buildMediaServeHandler()
	if err != nil {
		t.Fatalf("buildMediaServeHandler: %v", err)
	}
	go func() { errCh <- runStaticOrigin(ctx, addr, handler) }()
	waitForListening(t, addr)
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runStaticOrigin: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("static origin did not shut down")
	}
}

func TestRunStaticOrigin_InvalidAddr_ReturnsError(t *testing.T) {
	t.Parallel()
	handler, err := buildMediaServeHandler()
	if err != nil {
		t.Fatalf("buildMediaServeHandler: %v", err)
	}
	if err := runStaticOrigin(context.Background(), "not-a-valid-addr", handler); err == nil {
		t.Fatal("runStaticOrigin returned nil for invalid addr, want error")
	}
}

// TestExecuteAll_StaticOriginListenerStartsWhenConfigured asserts the
// static-origin listener is actually started by executeAllWith when
// STATIC_HTTP_ADDR is set, by verifying it accepts a TCP connection
// and answers the media route.
func TestExecuteAll_StaticOriginListenerStartsWhenConfigured(t *testing.T) {
	t.Parallel()
	publicAddr := freePort(t)
	staticAddr := freePort(t)
	getenv := func(k string) string {
		switch k {
		case envHTTPAddr:
			return publicAddr
		case envStaticOriginAddr:
			return staticAddr
		}
		return ""
	}
	dial := func(_ context.Context, _ func(string) string) (*dependencies, error) {
		return nil, errFakeDial
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codeCh := make(chan int, 1)
	go func() { codeCh <- executeAllWith(ctx, getenv, dial) }()

	waitForListening(t, publicAddr)
	waitForListening(t, staticAddr)

	// Hit the static origin and assert the static-origin headers are
	// present on the 404 response (path that doesn't resolve any media).
	res, err := http.Get("http://" + staticAddr + "/t/" + uuid.New().String() + "/logo")
	if err != nil {
		t.Fatalf("GET static origin: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusNotFound)
	}
	assertStaticOriginHeaders(t, res.Header)

	cancel()
	select {
	case <-codeCh:
	case <-time.After(5 * time.Second):
		t.Fatal("executeAllWith did not return after cancel")
	}
}

func multipartFile(t *testing.T, filename string, payload []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(uploadFormField, filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func assertStaticOriginHeaders(t *testing.T, h http.Header) {
	t.Helper()
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := h.Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
		t.Errorf("Cross-Origin-Resource-Policy = %q, want same-origin", got)
	}
	if got := h.Get("Content-Security-Policy"); got == "" {
		t.Error("Content-Security-Policy missing on static-origin response")
	}
	if got := h.Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin", got)
	}
}

// stubServeStore answers a single logo lookup; everything else is
// ErrNotFound. Keeps TestMediaServe_Logo_OkPath_SetsHeaders self-contained.
type stubServeStore struct {
	logo mediaserve.Media
}

func (s stubServeStore) LookupLogo(_ context.Context, tenantID uuid.UUID) (mediaserve.Media, error) {
	if s.logo.TenantID == uuid.Nil || s.logo.TenantID != tenantID {
		return mediaserve.Media{}, mediaserve.ErrNotFound
	}
	return s.logo, nil
}

func (s stubServeStore) LookupHash(context.Context, uuid.UUID, string) (mediaserve.Media, error) {
	return mediaserve.Media{}, mediaserve.ErrNotFound
}

type stubServeBlob struct {
	payload []byte
}

func (b stubServeBlob) Open(_ context.Context, _ string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(b.payload)), nil
}

// errFakeDial drives executeAllWith into the "internal listener
// disabled" branch so the smoke test does not need a Postgres or
// Redis stub for anything other than the static origin.
var errFakeDial = errFakeDialT("fake dial: not configured")

type errFakeDialT string

func (e errFakeDialT) Error() string { return string(e) }

// _ keeps the linter quiet about the unused net import; freePort uses
// net via cmd/server/main_test.go's helpers.
var _ = net.Listen

func TestNopMediaStorage_AlwaysNotFound(t *testing.T) {
	t.Parallel()
	s := nopMediaStorage{}
	if _, err := s.LookupHash(context.Background(), uuid.New(), strings.Repeat("a", 64)); err != mediaserve.ErrNotFound {
		t.Errorf("LookupHash err = %v, want ErrNotFound", err)
	}
	if _, err := s.LookupLogo(context.Background(), uuid.New()); err != mediaserve.ErrNotFound {
		t.Errorf("LookupLogo err = %v, want ErrNotFound", err)
	}
	if _, err := s.Open(context.Background(), "any/path"); err != mediaserve.ErrNotFound {
		t.Errorf("Open err = %v, want ErrNotFound", err)
	}
}

func TestMediaUpload_OversizedPNG_Returns413(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	registerUploadRoutes(mux)
	// 1500x1500 exceeds MaxWidth=1024 → ErrDimensionsExceeded → 413.
	body, ct := multipartFile(t, "logo.png", makeLargerPNG(t, 1500, 1500))
	r := httptest.NewRequest(http.MethodPost, "/api/tenant/uploads/logo", body)
	r.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestMediaUpload_GarbledPNG_Returns400(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	registerUploadRoutes(mux)
	// PNG magic bytes followed by garbage — sniff says PNG, decode fails.
	garbled := append([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x00}, []byte("garbage")...)
	body, ct := multipartFile(t, "x.png", garbled)
	r := httptest.NewRequest(http.MethodPost, "/api/tenant/uploads/logo", body)
	r.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func makeLargerPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x40, A: 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}
