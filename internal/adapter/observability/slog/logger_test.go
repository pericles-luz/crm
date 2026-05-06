package slog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	stdslog "log/slog"
	"testing"
	"time"

	logadapter "github.com/pericles-luz/crm/internal/adapter/observability/slog"
	"github.com/pericles-luz/crm/internal/webhook"
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", b, err)
	}
	return m
}

func TestLogResult_OmitsTenantIDPreAuth(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := logadapter.New(stdslog.New(stdslog.NewJSONHandler(&buf, &stdslog.HandlerOptions{Level: stdslog.LevelDebug})))

	logger.LogResult(context.Background(), webhook.LogRecord{
		RequestID:  "r1",
		Channel:    "whatsapp",
		Outcome:    webhook.OutcomeUnknownToken,
		ReceivedAt: time.Unix(1700000000, 0).UTC(),
		TenantID:   webhook.TenantID{0xaa},
	})
	rec := decode(t, buf.Bytes())
	if _, ok := rec["tenant_id"]; ok {
		t.Fatalf("pre-auth log must not contain tenant_id: %v", rec)
	}
	if rec["outcome"] != "unknown_token" {
		t.Fatalf("outcome = %v", rec["outcome"])
	}
}

func TestLogResult_IncludesTenantIDPostAuth(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := logadapter.New(stdslog.New(stdslog.NewJSONHandler(&buf, &stdslog.HandlerOptions{Level: stdslog.LevelDebug})))

	tenant := webhook.TenantID{0xaa}
	logger.LogResult(context.Background(), webhook.LogRecord{
		RequestID:   "r1",
		Channel:     "whatsapp",
		Outcome:     webhook.OutcomeAccepted,
		ReceivedAt:  time.Unix(1700000000, 0).UTC(),
		TenantID:    tenant,
		HasTenantID: true,
	})
	rec := decode(t, buf.Bytes())
	if rec["tenant_id"] == nil {
		t.Fatalf("post-auth log must contain tenant_id: %v", rec)
	}
}

func TestLogResult_EmitsErrorField(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := logadapter.New(stdslog.New(stdslog.NewJSONHandler(&buf, &stdslog.HandlerOptions{Level: stdslog.LevelDebug})))
	logger.LogResult(context.Background(), webhook.LogRecord{
		RequestID:  "r2",
		Channel:    "whatsapp",
		Outcome:    webhook.OutcomeSignatureInvalid,
		ReceivedAt: time.Unix(1700000000, 0).UTC(),
		Err:        errors.New("boom"),
	})
	rec := decode(t, buf.Bytes())
	if rec["error"] != "boom" {
		t.Fatalf("error field = %v", rec["error"])
	}
}

func TestNew_NilFallback(t *testing.T) {
	t.Parallel()
	logger := logadapter.New(nil)
	// Just exercise the path; default logger writes to stderr.
	logger.LogResult(context.Background(), webhook.LogRecord{Channel: "whatsapp", Outcome: webhook.OutcomeAccepted})
}
