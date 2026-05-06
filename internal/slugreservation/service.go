package slugreservation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Service is the use-case orchestrator. It holds only ports; the
// concrete adapters are wired in cmd/server.
type Service struct {
	store     Store
	redirects RedirectStore
	clock     Clock
	audit     MasterAuditLogger
	slack     SlackNotifier
}

// NewService validates required dependencies and returns a Service.
// Pass nil for clock to default to SystemClock; audit and slack are
// REQUIRED because the master override flow refuses to run without
// either ledger writable.
func NewService(store Store, redirects RedirectStore, audit MasterAuditLogger, slack SlackNotifier, clock Clock) (*Service, error) {
	if store == nil {
		return nil, errors.New("slugreservation: Store is required")
	}
	if redirects == nil {
		return nil, errors.New("slugreservation: RedirectStore is required")
	}
	if audit == nil {
		return nil, errors.New("slugreservation: MasterAuditLogger is required")
	}
	if slack == nil {
		return nil, errors.New("slugreservation: SlackNotifier is required")
	}
	if clock == nil {
		clock = SystemClock{}
	}
	return &Service{
		store:     store,
		redirects: redirects,
		clock:     clock,
		audit:     audit,
		slack:     slack,
	}, nil
}

// CheckAvailable returns nil if slug is free to claim. If an active
// reservation exists it returns *ReservedError wrapping ErrSlugReserved;
// callers can errors.As for the payload or errors.Is for the sentinel.
//
// CheckAvailable also normalizes the slug; passing a malformed value
// short-circuits to ErrInvalidSlug.
func (s *Service) CheckAvailable(ctx context.Context, rawSlug string) error {
	slug, err := NormalizeSlug(rawSlug)
	if err != nil {
		return err
	}
	res, err := s.store.Active(ctx, slug)
	if err != nil {
		if errors.Is(err, ErrNotReserved) {
			return nil
		}
		return fmt.Errorf("slugreservation: check available: %w", err)
	}
	return &ReservedError{Reservation: res}
}

// ReleaseSlug INSERTs a reservation row with expires_at = now + 365d.
// Called from tenant-delete, slug-change, and the suspension watcher.
// Idempotent at the partial-unique-index layer: if a non-expired
// reservation already exists for this slug the underlying Insert
// surfaces it via the unique-violation path (the adapter is expected
// to translate that to ErrSlugReserved).
func (s *Service) ReleaseSlug(ctx context.Context, rawSlug string, byTenantID uuid.UUID) (Reservation, error) {
	slug, err := NormalizeSlug(rawSlug)
	if err != nil {
		return Reservation{}, err
	}
	now := s.clock.Now().UTC()
	exp := now.Add(ReservationWindow)
	res, err := s.store.Insert(ctx, slug, byTenantID, now, exp)
	if err != nil {
		return Reservation{}, fmt.Errorf("slugreservation: release: %w", err)
	}
	return res, nil
}

// OverrideRelease soft-deletes the active reservation, emits the audit
// event, and posts a Slack alert. Slack failure is logged via audit
// (best-effort) but does not roll the override back — if the master
// authorized it and the DB committed, we keep the change.
//
// The caller is responsible for invoking this inside WithMasterOps so
// the master_ops_audit_trigger captures the row-level write.
func (s *Service) OverrideRelease(ctx context.Context, rawSlug string, masterID uuid.UUID, reason string) (Reservation, error) {
	if masterID == uuid.Nil {
		return Reservation{}, ErrZeroMaster
	}
	if strings.TrimSpace(reason) == "" {
		return Reservation{}, ErrReasonRequired
	}
	slug, err := NormalizeSlug(rawSlug)
	if err != nil {
		return Reservation{}, err
	}
	now := s.clock.Now().UTC()
	res, err := s.store.SoftDelete(ctx, slug, now)
	if err != nil {
		if errors.Is(err, ErrNotReserved) {
			return Reservation{}, ErrNotReserved
		}
		return Reservation{}, fmt.Errorf("slugreservation: override: %w", err)
	}
	ev := MasterOverrideEvent{
		Slug:     slug,
		MasterID: masterID,
		Reason:   strings.TrimSpace(reason),
		At:       now,
	}
	if err := s.audit.LogMasterOverride(ctx, ev); err != nil {
		return Reservation{}, fmt.Errorf("slugreservation: override audit: %w", err)
	}
	// Slack is best-effort; a downed webhook should not roll back the
	// master action. The audit log + master_ops_audit row are the
	// authoritative trail.
	_ = s.slack.NotifyAlert(ctx, formatSlackAlert(ev))
	return res, nil
}

// LookupRedirect returns the active redirect for old, or ErrNotReserved
// if none. Used by the subdomain catch-all handler.
func (s *Service) LookupRedirect(ctx context.Context, rawSlug string) (Redirect, error) {
	slug, err := NormalizeSlug(rawSlug)
	if err != nil {
		return Redirect{}, err
	}
	red, err := s.redirects.Active(ctx, slug)
	if err != nil {
		return Redirect{}, err
	}
	return red, nil
}

// RecordRedirect installs/updates a 12-month redirect from old to new.
// Slug-change orchestrators call this together with ReleaseSlug.
func (s *Service) RecordRedirect(ctx context.Context, oldRaw, newRaw string) (Redirect, error) {
	oldSlug, err := NormalizeSlug(oldRaw)
	if err != nil {
		return Redirect{}, err
	}
	newSlug, err := NormalizeSlug(newRaw)
	if err != nil {
		return Redirect{}, err
	}
	if oldSlug == newSlug {
		return Redirect{}, ErrInvalidSlug
	}
	exp := s.clock.Now().UTC().Add(RedirectWindow)
	red, err := s.redirects.Upsert(ctx, oldSlug, newSlug, exp)
	if err != nil {
		return Redirect{}, fmt.Errorf("slugreservation: record redirect: %w", err)
	}
	return red, nil
}

func formatSlackAlert(ev MasterOverrideEvent) string {
	return fmt.Sprintf(":rotating_light: *master slug-reservation override* — slug=`%s` master=`%s` reason=%q at=%s",
		ev.Slug, ev.MasterID, ev.Reason, ev.At.UTC().Format(time.RFC3339))
}

// Now exposes the service clock; test glue may use it to align
// generated payloads with expected timestamps.
func (s *Service) Now() time.Time { return s.clock.Now().UTC() }
