// Package slog adapts log/slog to webhook.Logger. The record shape is
// strict — the implementation never accepts arbitrary fields, which is
// the lint surface ADR §5 promises (no webhook_token, no raw_payload,
// no pre-HMAC tenant_id).
package slog

import (
	"context"
	"log/slog"

	"github.com/pericles-luz/crm/internal/webhook"
)

// Logger is the webhook.Logger implementation. It wraps a *slog.Logger
// so callers can attach handlers/sinks at composition time.
type Logger struct {
	l *slog.Logger
}

// New returns a Logger bound to base; passing nil falls back to
// slog.Default().
func New(base *slog.Logger) *Logger {
	if base == nil {
		base = slog.Default()
	}
	return &Logger{l: base}
}

// LogResult implements webhook.Logger.
func (lg *Logger) LogResult(ctx context.Context, rec webhook.LogRecord) {
	attrs := []slog.Attr{
		slog.String("request_id", rec.RequestID),
		slog.String("channel", rec.Channel),
		slog.String("outcome", string(rec.Outcome)),
		slog.Time("received_at", rec.ReceivedAt),
	}
	if rec.HasTenantID && rec.Outcome.IsAuthenticated() {
		attrs = append(attrs, slog.String("tenant_id", rec.TenantID.String()))
	}
	if rec.Err != nil {
		attrs = append(attrs, slog.String("error", rec.Err.Error()))
	}
	level := slog.LevelInfo
	if !rec.Outcome.IsAuthenticated() && rec.Outcome != webhook.OutcomeUnknownChannel {
		level = slog.LevelWarn
	}
	lg.l.LogAttrs(ctx, level, "webhook_request", attrs...)
}
