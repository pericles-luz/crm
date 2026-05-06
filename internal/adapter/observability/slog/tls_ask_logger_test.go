package slog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	crmslog "github.com/pericles-luz/crm/internal/adapter/observability/slog"
	"github.com/pericles-luz/crm/internal/customdomain/tls_ask"
)

func newJSONLogger(t *testing.T) (*crmslog.TLSAskLogger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	base := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return crmslog.NewTLSAskLogger(base), buf
}

func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestTLSAskLogger_AllowMessage(t *testing.T) {
	t.Parallel()
	lg, buf := newJSONLogger(t)
	lg.LogAllow(context.Background(), "shop.example.com")

	lines := decodeLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %s", len(lines), buf)
	}
	if lines[0]["msg"] != "customdomain.tls_ask_allow" {
		t.Fatalf("msg = %v", lines[0]["msg"])
	}
	if lines[0]["host"] != "shop.example.com" {
		t.Fatalf("host = %v", lines[0]["host"])
	}
}

func TestTLSAskLogger_DenyHasReasonAndHost(t *testing.T) {
	t.Parallel()
	lg, buf := newJSONLogger(t)
	lg.LogDeny(context.Background(), "evil.example.org", tls_ask.ReasonNotFound)

	lines := decodeLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %s", len(lines), buf)
	}
	rec := lines[0]
	if rec["msg"] != "customdomain.tls_ask_denied" {
		t.Fatalf("msg = %v", rec["msg"])
	}
	if rec["host"] != "evil.example.org" {
		t.Fatalf("host = %v", rec["host"])
	}
	if rec["reason"] != "not_found" {
		t.Fatalf("reason = %v", rec["reason"])
	}
}

func TestTLSAskLogger_ErrorIncludesUnderlying(t *testing.T) {
	t.Parallel()
	lg, buf := newJSONLogger(t)
	lg.LogError(context.Background(), "shop.example.com", tls_ask.ReasonRepositoryError, errors.New("connection refused"))

	lines := decodeLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %s", len(lines), buf)
	}
	rec := lines[0]
	if rec["msg"] != "customdomain.tls_ask_error" {
		t.Fatalf("msg = %v", rec["msg"])
	}
	if rec["reason"] != "repository_error" {
		t.Fatalf("reason = %v", rec["reason"])
	}
	if rec["error"] != "connection refused" {
		t.Fatalf("error = %v", rec["error"])
	}
}

func TestTLSAskLogger_ErrorWithNilUnderlyingDoesNotPanic(t *testing.T) {
	t.Parallel()
	lg, buf := newJSONLogger(t)
	lg.LogError(context.Background(), "shop.example.com", tls_ask.ReasonRepositoryError, nil)
	lines := decodeLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %s", len(lines), buf)
	}
	if _, ok := lines[0]["error"]; ok {
		t.Fatalf("error attribute present despite nil underlying: %v", lines[0])
	}
}

func TestTLSAskLogger_NilBaseUsesDefault(t *testing.T) {
	t.Parallel()
	lg := crmslog.NewTLSAskLogger(nil)
	// Should not panic.
	lg.LogAllow(context.Background(), "shop.example.com")
}
