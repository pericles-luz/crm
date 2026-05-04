package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	"github.com/pericles-luz/crm/internal/webhook"
)

func TestStubPublisher_AlwaysReturnsUnwiredError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	p := newStubPublisher(logger)
	err := p.Publish(context.Background(), [16]byte{}, webhook.TenantID{}, "whatsapp", []byte("{}"), nil)
	if !errors.Is(err, errStubPublisherUnwired) {
		t.Fatalf("err = %v, want errStubPublisherUnwired", err)
	}
	if !strings.Contains(buf.String(), "channel=whatsapp") {
		t.Fatalf("logger output missing channel: %q", buf.String())
	}
}

func TestStubPublisher_NilLoggerSafe(t *testing.T) {
	t.Parallel()
	p := newStubPublisher(nil)
	if err := p.Publish(context.Background(), [16]byte{}, webhook.TenantID{}, "facebook", nil, nil); !errors.Is(err, errStubPublisherUnwired) {
		t.Fatalf("err = %v", err)
	}
}

func TestStubJetStream_StreamInfoReturnsConfigured(t *testing.T) {
	t.Parallel()
	js := newStubJetStream("WEBHOOKS", time.Hour)
	cfg, err := js.StreamInfo(context.Background(), "WEBHOOKS")
	if err != nil {
		t.Fatalf("StreamInfo: %v", err)
	}
	if cfg.Name != "WEBHOOKS" || cfg.Duplicates != time.Hour {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestStubJetStream_StreamInfoUnknownStream(t *testing.T) {
	t.Parallel()
	js := newStubJetStream("WEBHOOKS", time.Hour)
	if _, err := js.StreamInfo(context.Background(), "OTHER"); err == nil {
		t.Fatal("expected error for unknown stream")
	}
}

func TestStubJetStream_PublishUnwired(t *testing.T) {
	t.Parallel()
	js := newStubJetStream("WEBHOOKS", time.Hour)
	if err := js.Publish(context.Background(), "subject", "id", nil); !errors.Is(err, errStubPublisherUnwired) {
		t.Fatalf("err = %v", err)
	}
}

func TestStubJetStream_FailsValidationWhenWindowTooSmall(t *testing.T) {
	t.Parallel()
	js := newStubJetStream("WEBHOOKS", 30*time.Minute)
	if err := nats.ValidateStream(context.Background(), js, "WEBHOOKS"); err == nil {
		t.Fatal("expected ValidateStream to fail on Duplicates<1h")
	}
}

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
