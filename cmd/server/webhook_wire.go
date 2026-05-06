package main

// SIN-62300 wiring — public-side webhook intake + reconciler worker.
//
// The webhook stack (internal/webhook + internal/worker, mergeados via
// SIN-62297) é composto de bibliotecas com testes verdes mas até esta
// issue não estava ligado ao runtime de cmd/server. Este arquivo conecta
// as peças seguindo o mesmo padrão de gating env-driven que
// customdomain_wire.go: o feature flag WEBHOOK_ENABLED=1 + DATABASE_URL
// + pelo menos um secret de canal Meta habilitam:
//
//   - rota POST /webhooks/{channel}/{webhook_token} no mux público;
//   - goroutine de worker.Reconciler com shutdown ordenado pelo context
//     do processo (cancel → drena HTTP → espera reconciler).
//
// O publisher de NATS e o pg-backed UnpublishedSource não estão neste PR
// (rastreado em follow-ups). O wire-up usa stubs no-op com WARN no log
// para que cmd/server permaneça testável e para tornar o gap visível em
// produção; flipar WEBHOOK_ENABLED=1 num ambiente customer-facing exige
// trocar esses stubs antes do go-live (ver ADR 0075 §6 reversibilidade).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	metaadapter "github.com/pericles-luz/crm/internal/adapter/channel/meta"
	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	promobs "github.com/pericles-luz/crm/internal/adapter/observability/prometheus"
	slogobs "github.com/pericles-luz/crm/internal/adapter/observability/slog"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	httpadapter "github.com/pericles-luz/crm/internal/adapter/transport/http"
	"github.com/pericles-luz/crm/internal/webhook"
	"github.com/pericles-luz/crm/internal/worker"
)

const (
	envWebhookEnabled             = "WEBHOOK_ENABLED"
	envWebhookMetaWhatsAppSecret  = "META_WHATSAPP_APP_SECRET"
	envWebhookMetaInstagramSecret = "META_INSTAGRAM_APP_SECRET"
	envWebhookMetaFacebookSecret  = "META_FACEBOOK_APP_SECRET"
)

// webhookPool is the pgx-shaped surface buildWebhookWiring needs from
// the pool. *pgxpool.Pool satisfies it via pgstore.PgxConn + Close.
type webhookPool interface {
	pgstore.PgxConn
	Close()
}

// webhookDial is the test seam: production opens a real pgxpool; tests
// inject a fake that satisfies pgstore.PgxConn.
type webhookDial func(ctx context.Context, dsn string) (webhookPool, error)

func defaultWebhookDial(ctx context.Context, dsn string) (webhookPool, error) {
	pool, err := pgpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return pool, nil
}

// webhookWiring bundles the artifacts needed by run()/executeAllWith to
// mount the webhook intake and drive the reconciler worker.
type webhookWiring struct {
	// Register attaches the webhook handler routes onto an existing mux.
	// Caller is responsible for choosing the mux (typically the public
	// listener mux).
	Register func(*http.ServeMux)
	// RunWorker blocks running the reconciler until ctx is cancelled.
	// Returns nil on graceful shutdown (ctx.Done) and a non-nil error
	// only on unrecoverable startup misconfiguration — sweep errors are
	// non-fatal and retried on the next tick.
	RunWorker func(context.Context) error
	// Cleanup releases the pool/clients held by the wiring. Safe to call
	// after RunWorker has returned.
	Cleanup func()
}

// buildWebhookWiring is the production entry point used by run().
func buildWebhookWiring(ctx context.Context, getenv func(string) string) *webhookWiring {
	return buildWebhookWiringWithDeps(ctx, getenv, defaultWebhookDial)
}

// buildWebhookWiringWithDeps is the test seam — accepts an injectable
// pool dialer so unit tests can drive the wiring without Postgres.
func buildWebhookWiringWithDeps(ctx context.Context, getenv func(string) string, dial webhookDial) *webhookWiring {
	if getenv(envWebhookEnabled) != "1" {
		return nil
	}
	dsn := getenv(pgpool.EnvDSN)
	if dsn == "" {
		log.Printf("crm: webhook intake disabled (DATABASE_URL unset)")
		return nil
	}

	adapters, err := buildMetaAdapters(getenv)
	if err != nil {
		log.Printf("crm: webhook intake disabled — adapter: %v", err)
		return nil
	}
	if len(adapters) == 0 {
		log.Printf("crm: webhook intake disabled (no Meta channel app-secret env vars set)")
		return nil
	}

	pool, err := dial(ctx, dsn)
	if err != nil {
		log.Printf("crm: webhook intake disabled — pg connect: %v", err)
		return nil
	}

	tokens := pgstore.NewTokenStore(pool)
	idem := pgstore.NewIdempotencyStore(pool)
	rawEvents := pgstore.NewRawEventStore(pool)
	assoc := pgstore.NewTenantAssociationStore(pool)

	publisher := newNoopPublisher()
	src := newNoopUnpublishedSource()

	// Each cmd/server bring-up gets its own Registry so that a process
	// restart (or a unit test running buildWebhookWiring twice) does not
	// trip the duplicate-registration panic in client_golang.
	metrics := promobs.New(prometheus.NewRegistry())
	logger := slogobs.New(slog.Default())

	svc, err := webhook.NewService(webhook.Config{
		Adapters:               adapters,
		TokenStore:             tokens,
		IdempotencyStore:       idem,
		RawEventStore:          rawEvents,
		TenantAssociationStore: assoc,
		Publisher:              publisher,
		Logger:                 logger,
		Metrics:                metrics,
	})
	if err != nil {
		pool.Close()
		log.Printf("crm: webhook intake disabled — service: %v", err)
		return nil
	}

	rec, err := worker.New(worker.Config{
		Source:    src,
		Publisher: publisher,
		RawEvents: rawEvents,
	})
	if err != nil {
		pool.Close()
		log.Printf("crm: webhook intake disabled — reconciler: %v", err)
		return nil
	}

	handler := httpadapter.NewHandler(svc)
	register := func(mux *http.ServeMux) {
		handler.Register(mux)
	}

	runWorker := func(c context.Context) error {
		if err := rec.Run(c); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	}
	cleanup := func() { pool.Close() }
	return &webhookWiring{
		Register:  register,
		RunWorker: runWorker,
		Cleanup:   cleanup,
	}
}

// buildMetaAdapters constructs one Meta adapter per (channel, env-var)
// pair that has a non-empty secret. Empty list = caller disables the
// wiring (the Meta intake has nothing to verify against).
func buildMetaAdapters(getenv func(string) string) ([]webhook.ChannelAdapter, error) {
	pairs := []struct {
		channel string
		env     string
	}{
		{"whatsapp", envWebhookMetaWhatsAppSecret},
		{"instagram", envWebhookMetaInstagramSecret},
		{"facebook", envWebhookMetaFacebookSecret},
	}
	out := make([]webhook.ChannelAdapter, 0, len(pairs))
	for _, p := range pairs {
		secret := getenv(p.env)
		if secret == "" {
			continue
		}
		a, err := metaadapter.New(p.channel, secret)
		if err != nil {
			return nil, fmt.Errorf("meta %s: %w", p.channel, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// noopPublisher swallows publish requests until a real NATS-backed
// publisher replaces it. A startup WARN flags the gap; production
// deploys MUST swap before flipping WEBHOOK_ENABLED=1 in a customer-
// facing environment.
type noopPublisher struct{}

func newNoopPublisher() *noopPublisher {
	slog.Default().Warn(
		"webhook: publisher running in no-op mode; events are accepted and stored but never delivered downstream. Swap to NATS adapter before flipping WEBHOOK_ENABLED=1 in production.",
		slog.String("component", "webhook.publisher"),
	)
	return &noopPublisher{}
}

func (*noopPublisher) Publish(context.Context, [16]byte, webhook.TenantID, string, []byte, map[string][]string) error {
	return nil
}

// noopUnpublishedSource always returns an empty batch so the reconciler
// exercises its lifecycle (start, tick, stop) without touching storage.
// The pg-backed adapter that scans `raw_event WHERE published_at IS NULL`
// is a follow-up; the WARN ensures the gap is visible in logs.
type noopUnpublishedSource struct{}

func newNoopUnpublishedSource() *noopUnpublishedSource {
	slog.Default().Warn(
		"webhook: reconciler source running in no-op mode; unpublished raw_event rows are NOT retried. Swap to a postgres-backed UnpublishedSource before flipping WEBHOOK_ENABLED=1 in production.",
		slog.String("component", "webhook.reconciler.source"),
	)
	return &noopUnpublishedSource{}
}

func (*noopUnpublishedSource) FetchUnpublished(context.Context, time.Time, int) ([]worker.UnpublishedRow, error) {
	return nil, nil
}

// Compile-time guards.
var (
	_ webhook.EventPublisher   = (*noopPublisher)(nil)
	_ worker.UnpublishedSource = (*noopUnpublishedSource)(nil)
)
