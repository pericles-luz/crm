package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthHandler_ReturnsOKJSON(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	healthHandler(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body[status] = %q, want %q", body["status"], "ok")
	}
}

func TestNewMux_RoutesHealth(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}
}

func TestRun_ShutsDownOnContextCancel(t *testing.T) {
	t.Parallel()
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, addr) }()

	waitForListening(t, addr)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}

func TestRun_ReturnsErrorOnInvalidAddr(t *testing.T) {
	t.Parallel()
	if err := run(context.Background(), "not-a-valid-addr"); err == nil {
		t.Fatal("run with invalid addr returned nil, want error")
	}
}

func TestExecute_ReturnsZeroOnGracefulShutdown(t *testing.T) {
	t.Parallel()
	addr := freePort(t)
	getenv := func(k string) string {
		if k == "HTTP_ADDR" {
			return addr
		}
		return ""
	}
	ctx, cancel := context.WithCancel(context.Background())
	codeCh := make(chan int, 1)
	go func() { codeCh <- execute(ctx, getenv) }()

	waitForListening(t, addr)
	cancel()

	select {
	case code := <-codeCh:
		if code != 0 {
			t.Fatalf("execute returned %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("execute did not return after cancel")
	}
}

func TestExecute_ReturnsOneOnRunError(t *testing.T) {
	t.Parallel()
	getenv := func(string) string { return "definitely-not-an-addr" }
	if code := execute(context.Background(), getenv); code != 1 {
		t.Fatalf("execute returned %d, want 1", code)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func waitForListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("server did not listen on %s", addr)
}

// --- SIN-62258 upload UI route tests -----------------------------------

func TestNewMux_RoutesLogoUploadForm(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/uploads/logo", nil)
	newMux().ServeHTTP(rec, req)

	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", got)
	}
	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		`data-upload="logo"`,
		`accept="image/png,image/jpeg,image/webp"`,
		`Logo da empresa`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("logo form response missing %q", want)
		}
	}
}

func TestNewMux_AttachmentForm404WhenFlagOff(t *testing.T) {
	t.Setenv("SIN_UPLOAD_ATTACHMENT_FORM", "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/uploads/attachment", nil)
	newMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (flag off)", rec.Code)
	}
}

func TestNewMux_AttachmentForm200WhenFlagOn(t *testing.T) {
	t.Setenv("SIN_UPLOAD_ATTACHMENT_FORM", "1")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/uploads/attachment", nil)
	newMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (flag on)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-upload="attachment"`) {
		t.Errorf("attachment form response missing data-upload marker")
	}
	if !strings.Contains(body, "Anexo (PNG, JPG, WEBP ou PDF)") {
		t.Errorf("attachment form response missing PT-BR label")
	}
}

func TestNewMux_AttachmentForm_FlagAcceptsTrueLiteral(t *testing.T) {
	t.Setenv("SIN_UPLOAD_ATTACHMENT_FORM", "true")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/uploads/attachment", nil)
	newMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for SIN_UPLOAD_ATTACHMENT_FORM=true", rec.Code)
	}
}

func TestNewMux_LogoFormRejectsPOSTWithAllowHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/uploads/logo", nil)
	newMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Errorf("Allow = %q, want GET", got)
	}
}

func TestNewMux_StaticUploadJS(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/static/upload/upload.js")
	if err != nil {
		t.Fatalf("GET upload.js: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "SIN-62258") {
		t.Errorf("upload.js missing SIN-62258 marker — wrong file served? got %d bytes", len(body))
	}
}

func TestNewMux_StaticUploadCSS(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/static/upload/upload.css")
	if err != nil {
		t.Fatalf("GET upload.css: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
}

func TestUploadAttachmentFormEnabled(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{"1", true},
		{"true", true},
		{"True", true},
		{"  true  ", true},
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			t.Setenv("SIN_UPLOAD_ATTACHMENT_FORM", c.val)
			if got := uploadAttachmentFormEnabled(); got != c.want {
				t.Errorf("env=%q got %v, want %v", c.val, got, c.want)
			}
		})
	}
}
