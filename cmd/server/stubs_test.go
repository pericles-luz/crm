package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"context"
)

func TestStubUnpublishedSource_ReturnsNil(t *testing.T) {
	t.Parallel()
	rows, err := stubUnpublishedSource{}.FetchUnpublished(context.Background(), time.Now(), 100)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if rows != nil {
		t.Fatalf("rows = %v, want nil", rows)
	}
}

func TestStubWebhookHandler_Returns200JSON(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("POST /webhooks/{channel}/{webhook_token}", stubWebhookHandler(slog.New(slog.NewTextHandler(io.Discard, nil))))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhooks/whatsapp/some-token", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body = %v", body)
	}
}

func TestStubWebhookHandler_NilLoggerSafe(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/whatsapp/tok", nil)
	req.SetPathValue("channel", "whatsapp")
	stubWebhookHandler(nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
}
