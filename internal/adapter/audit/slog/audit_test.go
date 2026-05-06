package slog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	auditslog "github.com/pericles-luz/crm/internal/adapter/audit/slog"
	"github.com/pericles-luz/crm/internal/slugreservation"
)

func TestLogMasterOverride_EmitsStructuredLine(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	a := auditslog.New(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	masterID := uuid.New()
	at := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)

	if err := a.LogMasterOverride(context.Background(), slugreservation.MasterOverrideEvent{
		Slug:     "acme",
		MasterID: masterID,
		Reason:   "incident #42",
		At:       at,
	}); err != nil {
		t.Fatalf("LogMasterOverride: %v", err)
	}

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec["msg"] != auditslog.MasterAuditEvent {
		t.Fatalf("msg=%v", rec["msg"])
	}
	if rec["event"] != auditslog.MasterAuditEvent {
		t.Fatalf("event=%v", rec["event"])
	}
	if rec["slug"] != "acme" || rec["reason"] != "incident #42" {
		t.Fatalf("rec=%+v", rec)
	}
	if rec["master_id"] != masterID.String() {
		t.Fatalf("master_id=%v", rec["master_id"])
	}
}

func TestLogMasterOverride_NilLogger(t *testing.T) {
	t.Parallel()
	var a *auditslog.Audit
	if err := a.LogMasterOverride(context.Background(), slugreservation.MasterOverrideEvent{}); err == nil {
		t.Fatal("expected error on nil receiver")
	}
}

func TestNewWithNilFallsBackToDefault(t *testing.T) {
	t.Parallel()
	a := auditslog.New(nil)
	if a == nil {
		t.Fatal("nil")
	}
	if err := a.LogMasterOverride(context.Background(), slugreservation.MasterOverrideEvent{Slug: "x", MasterID: uuid.New()}); err != nil {
		t.Fatalf("LogMasterOverride: %v", err)
	}
}
