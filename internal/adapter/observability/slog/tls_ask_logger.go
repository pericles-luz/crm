package slog

import (
	"context"
	"log/slog"

	"github.com/pericles-luz/crm/internal/customdomain/tls_ask"
)

// TLSAskLogger adapts log/slog to tls_ask.Logger. It emits the structured
// events the F45 acceptance criteria call out — `customdomain.tls_ask_denied`
// in particular — with stable `host` and `reason` keys so dashboards stay
// dimensionally compatible across releases.
type TLSAskLogger struct {
	l *slog.Logger
}

// NewTLSAskLogger wraps base; nil falls back to slog.Default().
func NewTLSAskLogger(base *slog.Logger) *TLSAskLogger {
	if base == nil {
		base = slog.Default()
	}
	return &TLSAskLogger{l: base}
}

// LogAllow emits at INFO. Allow-side is rare under normal load (Caddy
// caches issued certs) so we keep the log line.
func (lg *TLSAskLogger) LogAllow(ctx context.Context, host string) {
	lg.l.LogAttrs(ctx, slog.LevelInfo, "customdomain.tls_ask_allow",
		slog.String("host", host),
	)
}

// LogDeny emits at INFO with the structured reason key. The acceptance
// criteria pin `customdomain.tls_ask_denied{host, reason}` as the message.
func (lg *TLSAskLogger) LogDeny(ctx context.Context, host string, reason tls_ask.Reason) {
	lg.l.LogAttrs(ctx, slog.LevelInfo, "customdomain.tls_ask_denied",
		slog.String("host", host),
		slog.String("reason", reason.String()),
	)
}

// LogError emits at ERROR with the underlying error message. Used for
// transient port faults that the use-case maps to HTTP 5xx.
func (lg *TLSAskLogger) LogError(ctx context.Context, host string, reason tls_ask.Reason, err error) {
	attrs := []slog.Attr{
		slog.String("host", host),
		slog.String("reason", reason.String()),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	lg.l.LogAttrs(ctx, slog.LevelError, "customdomain.tls_ask_error", attrs...)
}

// Compile-time guard.
var _ tls_ask.Logger = (*TLSAskLogger)(nil)
