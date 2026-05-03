package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
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
