// Package worker hosts background workers that operate on the webhook
// pipeline outside the request path. The reconciler in this file
// implements ADR 0075 §2 D7: every 30s, sweep raw_event rows where
// published_at IS NULL and received_at < now() - 1m, attempt to publish,
// and mark them on success.
package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pericles-luz/crm/internal/webhook"
)

// UnpublishedRow is the per-row payload the reconciler hands to the
// publisher. We re-parse jsonb headers via a thin adapter rather than
// importing pgtype here to keep the surface minimal.
type UnpublishedRow struct {
	ID       [16]byte
	TenantID webhook.TenantID
	Channel  string
	Payload  []byte
	Headers  map[string][]string
	Received time.Time
}

// Reconciler scans for webhook events that landed in raw_event but
// never had their published_at marked, and re-publishes them. It is
// safe to run multiple replicas: SELECT … FOR UPDATE SKIP LOCKED
// partitions work between replicas at row granularity.
type Reconciler struct {
	src        UnpublishedSource
	publisher  webhook.EventPublisher
	rawEvents  webhook.RawEventStore
	clock      webhook.Clock
	tickEvery  time.Duration
	staleAfter time.Duration
	alertAfter time.Duration
	batchSize  int
	onStale    func(eventID [16]byte, age time.Duration) // nil-safe alerter hook
}

// UnsableSource is the read side that yields candidate rows. Splitting
// it from RawEventStore keeps the reconciler unit-testable without a
// real DB and avoids inflating the public RawEventStore port with
// reconciler-only methods.
type UnpublishedSource interface {
	FetchUnpublished(ctx context.Context, olderThan time.Time, limit int) ([]UnpublishedRow, error)
}

// Config bundles reconciler dependencies. Defaults match ADR §2 D7.
type Config struct {
	Source     UnpublishedSource
	Publisher  webhook.EventPublisher
	RawEvents  webhook.RawEventStore
	Clock      webhook.Clock
	TickEvery  time.Duration // default 30s
	StaleAfter time.Duration // default 1m
	AlertAfter time.Duration // default 1h — alert above this age
	BatchSize  int           // default 100
	OnStale    func(eventID [16]byte, age time.Duration)
}

// New returns a Reconciler ready to Run. Required fields: Source,
// Publisher, RawEvents.
func New(cfg Config) (*Reconciler, error) {
	if cfg.Source == nil {
		return nil, errors.New("worker: Source is required")
	}
	if cfg.Publisher == nil {
		return nil, errors.New("worker: Publisher is required")
	}
	if cfg.RawEvents == nil {
		return nil, errors.New("worker: RawEvents is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = webhook.SystemClock{}
	}
	if cfg.TickEvery == 0 {
		cfg.TickEvery = 30 * time.Second
	}
	if cfg.StaleAfter == 0 {
		cfg.StaleAfter = time.Minute
	}
	if cfg.AlertAfter == 0 {
		cfg.AlertAfter = time.Hour
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 100
	}
	return &Reconciler{
		src:        cfg.Source,
		publisher:  cfg.Publisher,
		rawEvents:  cfg.RawEvents,
		clock:      cfg.Clock,
		tickEvery:  cfg.TickEvery,
		staleAfter: cfg.StaleAfter,
		alertAfter: cfg.AlertAfter,
		batchSize:  cfg.BatchSize,
		onStale:    cfg.OnStale,
	}, nil
}

// Run blocks until ctx is done, ticking once per TickEvery. Errors from
// individual sweeps are non-fatal — the next tick retries.
func (r *Reconciler) Run(ctx context.Context) error {
	t := time.NewTicker(r.tickEvery)
	defer t.Stop()
	// Run one immediate pass so tests don't need to wait the first tick.
	_ = r.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			_ = r.tick(ctx)
		}
	}
}

// Tick performs a single sweep. Exported for tests and CLI tools.
func (r *Reconciler) Tick(ctx context.Context) error { return r.tick(ctx) }

func (r *Reconciler) tick(ctx context.Context) error {
	now := r.clock.Now()
	older := now.Add(-r.staleAfter)
	rows, err := r.src.FetchUnpublished(ctx, older, r.batchSize)
	if err != nil {
		return fmt.Errorf("fetch unpublished: %w", err)
	}
	for _, row := range rows {
		age := now.Sub(row.Received)
		if r.alertAfter > 0 && age >= r.alertAfter && r.onStale != nil {
			r.onStale(row.ID, age)
		}
		if err := r.publisher.Publish(ctx, row.ID, row.TenantID, row.Channel, row.Payload, row.Headers); err != nil {
			continue // next tick retries
		}
		_ = r.rawEvents.MarkPublished(ctx, row.ID, r.clock.Now())
	}
	return nil
}
