package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/pericles-luz/crm/internal/adapter/channel/meta"
	"github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	promobs "github.com/pericles-luz/crm/internal/adapter/observability/prometheus"
	slogobs "github.com/pericles-luz/crm/internal/adapter/observability/slog"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	transporthttp "github.com/pericles-luz/crm/internal/adapter/transport/http"
	"github.com/pericles-luz/crm/internal/webhook"
	"github.com/pericles-luz/crm/internal/worker"
)

// stack is the runtime composition of the webhook subsystem produced by
// buildStack. The handler is always non-nil; reconciler is non-nil only
// when the security_v2 feature flag is enabled.
type stack struct {
	handler    http.Handler
	reconciler *worker.Reconciler
	probe      *probedSource
	registry   *prometheus.Registry
	closer     func()
	enabled    bool
	cfg        config
}

// Close releases per-stack resources (pgx pool, etc.).
func (s *stack) Close() {
	if s == nil || s.closer == nil {
		return
	}
	s.closer()
}

// poolOpener is the seam that lets tests skip a real Postgres connection.
type poolOpener func(ctx context.Context, url string) (pgstore.PgxConn, func(), error)

func defaultPoolOpener(ctx context.Context, url string) (pgstore.PgxConn, func(), error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, nil, fmt.Errorf("postgres pool: %w", err)
	}
	return pool, pool.Close, nil
}

// buildStack constructs the webhook stack from cfg. With the feature flag
// off (default) it returns a stub handler that always answers 200 OK and
// no reconciler. With the flag on it validates JetStream stream config,
// opens a pgx pool, registers Meta adapters, and prepares the reconciler
// to be started by the caller via reconciler.Run on a goroutine.
func buildStack(ctx context.Context, cfg config, logger *slog.Logger, openPool poolOpener) (*stack, error) {
	if !cfg.WebhookV2Enabled {
		return &stack{
			handler: stubWebhookHandler(logger),
			cfg:     cfg,
		}, nil
	}

	if cfg.NATSValidateStream {
		js := newStubJetStream(cfg.NATSStreamName, cfg.NATSStreamDuplicatesWindow)
		if err := nats.ValidateStream(ctx, js, cfg.NATSStreamName); err != nil {
			return nil, err
		}
	}

	if openPool == nil {
		openPool = defaultPoolOpener
	}
	pool, closer, err := openPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	adapters := make([]webhook.ChannelAdapter, 0, len(cfg.MetaChannels))
	for _, ch := range cfg.MetaChannels {
		a, err := meta.New(ch, cfg.MetaAppSecret)
		if err != nil {
			closer()
			return nil, fmt.Errorf("meta adapter %q: %w", ch, err)
		}
		adapters = append(adapters, a)
	}

	registry := prometheus.NewRegistry()
	metrics := promobs.New(registry)
	wlog := slogobs.New(logger)
	publisher := newStubPublisher(logger)
	rawEvents := pgstore.NewRawEventStore(pool)

	svc, err := webhook.NewService(webhook.Config{
		Adapters:               adapters,
		TokenStore:             pgstore.NewTokenStore(pool),
		IdempotencyStore:       pgstore.NewIdempotencyStore(pool),
		RawEventStore:          rawEvents,
		Publisher:              publisher,
		TenantAssociationStore: pgstore.NewTenantAssociationStore(pool),
		Clock:                  webhook.SystemClock{},
		Logger:                 wlog,
		Metrics:                metrics,
	})
	if err != nil {
		closer()
		return nil, fmt.Errorf("webhook service: %w", err)
	}

	probe := newProbedSource(stubUnpublishedSource{})
	rec, err := worker.New(worker.Config{
		Source:     probe,
		Publisher:  publisher,
		RawEvents:  rawEvents,
		Clock:      webhook.SystemClock{},
		TickEvery:  cfg.ReconcilerTickEvery,
		StaleAfter: cfg.ReconcilerStaleAfter,
		AlertAfter: cfg.ReconcilerAlertAfter,
		BatchSize:  cfg.ReconcilerBatchSize,
	})
	if err != nil {
		closer()
		return nil, fmt.Errorf("reconciler: %w", err)
	}

	return &stack{
		handler:    transporthttp.NewHandler(svc),
		reconciler: rec,
		probe:      probe,
		registry:   registry,
		closer:     closer,
		enabled:    true,
		cfg:        cfg,
	}, nil
}

// buildMux assembles the production routes from a stack: the
// reconciler-aware /health, /metrics (when the feature flag is on so the
// recorder has a registry), and the webhook intake handler.
func buildMux(s *stack) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/health", healthHandlerFor(s.probe, s.cfg.ReconcilerHealthStaleness, nil))
	if s.registry != nil {
		mux.Handle("GET /metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))
	}
	mux.Handle("POST /webhooks/{channel}/{webhook_token}", s.handler)
	return mux
}
