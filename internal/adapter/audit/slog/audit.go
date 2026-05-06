// Package slog adapts log/slog to slugreservation.MasterAuditLogger.
//
// We emit a single structured log record at INFO level with the
// machine-grep-able event tag `master_slug_reservation_overridden`.
// The handler the binary chose (JSON in prod, text in dev) decides the
// wire shape; we only set the attrs the audit consumer needs.
//
// The DB-level master_ops_audit row already captures the row-level
// UPDATE; this package writes the human-readable layer (master id +
// reason) that the audit table cannot store.
package slog

import (
	"context"
	"errors"
	"log/slog"

	"github.com/pericles-luz/crm/internal/slugreservation"
)

// MasterAuditEvent is the slog event name. Documented as a constant so
// alerting/queries elsewhere can rely on a stable string.
const MasterAuditEvent = "master_slug_reservation_overridden"

// Audit is the slog-backed slugreservation.MasterAuditLogger.
type Audit struct {
	l *slog.Logger
}

// New returns an Audit bound to base; nil falls back to slog.Default.
func New(base *slog.Logger) *Audit {
	if base == nil {
		base = slog.Default()
	}
	return &Audit{l: base}
}

// LogMasterOverride implements slugreservation.MasterAuditLogger.
func (a *Audit) LogMasterOverride(ctx context.Context, ev slugreservation.MasterOverrideEvent) error {
	if a == nil || a.l == nil {
		return errors.New("audit: not initialised")
	}
	a.l.LogAttrs(ctx, slog.LevelInfo, MasterAuditEvent,
		slog.String("event", MasterAuditEvent),
		slog.String("slug", ev.Slug),
		slog.String("master_id", ev.MasterID.String()),
		slog.String("reason", ev.Reason),
		slog.Time("at", ev.At),
	)
	return nil
}
