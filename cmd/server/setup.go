package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"os"

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

// publisherFactory is the seam that lets tests inject a fake
// webhook.EventPublisher without dialing a real NATS server. The
// returned closer drains the underlying NATS connection (or is a
// no-op for fakes) and is composed into stack.closer.
type publisherFactory func(ctx context.Context, cfg config, logger *slog.Logger) (webhook.EventPublisher, func(), error)

func defaultPublisherFactory(ctx context.Context, cfg config, _ *slog.Logger) (webhook.EventPublisher, func(), error) {
	tlsCfg, err := loadNATSTLSConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("nats tls: %w", err)
	}
	adp, err := nats.Connect(ctx, nats.SDKConfig{
		URL:           cfg.NATSURL,
		ClientName:    cfg.NATSClientName,
		CredsFile:     cfg.NATSCredsFile,
		TLSConfig:     tlsCfg,
		ReconnectWait: cfg.NATSReconnectWait,
		MaxReconnects: cfg.NATSMaxReconnects,
	})
	if err != nil {
		return nil, nil, err
	}
	pub, err := nats.New(ctx, adp, cfg.NATSStreamName, cfg.NATSSubjectPrefix)
	if err != nil {
		adp.Close()
		return nil, nil, err
	}
	return pub, adp.Close, nil
}

// loadNATSTLSConfig builds a *tls.Config from cfg only when at least
// one TLS knob is set. Returns nil otherwise so the SDK falls back to
// its scheme-based default (tls:// URL still negotiates TLS without an
// explicit config).
func loadNATSTLSConfig(cfg config) (*tls.Config, error) {
	if cfg.NATSTLSCAFile == "" && cfg.NATSTLSCertFile == "" && cfg.NATSTLSKeyFile == "" && cfg.NATSTLSServerName == "" {
		return nil, nil
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.NATSTLSServerName}
	if cfg.NATSTLSCAFile != "" {
		caPEM, err := os.ReadFile(cfg.NATSTLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file %q: %w", cfg.NATSTLSCAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("parse CA file %q: no certificates found", cfg.NATSTLSCAFile)
		}
		tlsCfg.RootCAs = pool
	}
	if cfg.NATSTLSCertFile != "" && cfg.NATSTLSKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.NATSTLSCertFile, cfg.NATSTLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client keypair: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return tlsCfg, nil
}

// buildStack constructs the webhook stack from cfg. With the feature flag
// off (default) it returns a stub handler that always answers 200 OK and
// no reconciler. With the flag on it dials NATS and constructs the real
// JetStream publisher (which validates the stream's Duplicates >= 1h
// window eagerly per ADR 0075 rev 3 / F-14), opens a pgx pool, registers
// Meta adapters, and prepares the reconciler to be started by the caller
// via reconciler.Run on a goroutine.
func buildStack(ctx context.Context, cfg config, logger *slog.Logger, openPool poolOpener, newPublisher publisherFactory) (*stack, error) {
	if !cfg.WebhookV2Enabled {
		return &stack{
			handler: stubWebhookHandler(logger),
			cfg:     cfg,
		}, nil
	}

	if openPool == nil {
		openPool = defaultPoolOpener
	}
	if newPublisher == nil {
		newPublisher = defaultPublisherFactory
	}

	pool, poolClose, err := openPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	publisher, pubClose, err := newPublisher(ctx, cfg, logger)
	if err != nil {
		poolClose()
		return nil, fmt.Errorf("nats publisher: %w", err)
	}
	closer := chainClosers(pubClose, poolClose)

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

// chainClosers runs each non-nil closer in the order given and returns
// nil if no closers are passed.
func chainClosers(closers ...func()) func() {
	if len(closers) == 0 {
		return nil
	}
	return func() {
		for _, c := range closers {
			if c != nil {
				c()
			}
		}
	}
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
