package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	"github.com/pericles-luz/crm/internal/webhook"
	"github.com/pericles-luz/crm/internal/worker"
)

// errStubPublisherUnwired is returned by stubPublisher.Publish so the
// service treats every event as "publish failed" and the reconciler keeps
// retrying. This is the safe default while the NATS SDK adapter
// (SIN-62274 child issue) is not yet wired: rows in raw_event remain
// published_at=NULL forever, never silently lost.
var errStubPublisherUnwired = errors.New("nats: SDK publisher not wired (child issue pending)")

// stubPublisher implements webhook.EventPublisher with a logged failure.
// It exists so the wire-up can construct a webhook.Service today; the
// real NATS SDK adapter replaces it on the SIN-62274 follow-up.
type stubPublisher struct {
	logger *slog.Logger
}

func newStubPublisher(logger *slog.Logger) *stubPublisher {
	return &stubPublisher{logger: logger}
}

func (p *stubPublisher) Publish(_ context.Context, _ [16]byte, _ webhook.TenantID, channel string, _ []byte, _ map[string][]string) error {
	if p.logger != nil {
		p.logger.Warn("nats publisher stub — event left unpublished",
			slog.String("channel", channel),
		)
	}
	return errStubPublisherUnwired
}

// stubJetStream returns the operator-configured StreamConfig so
// nats.ValidateStream can run its F-14 (Duplicates ≥ 1h) check at startup
// even before the real NATS SDK adapter is wired. Operators set
// NATS_STREAM_DUPLICATES_WINDOW to mirror the production stream config;
// a deploy with Duplicates < 1h fails-fast at startup and never serves.
type stubJetStream struct {
	cfg nats.StreamConfig
}

func newStubJetStream(name string, duplicates time.Duration) *stubJetStream {
	return &stubJetStream{cfg: nats.StreamConfig{Name: name, Duplicates: duplicates}}
}

func (j *stubJetStream) StreamInfo(_ context.Context, name string) (nats.StreamConfig, error) {
	if name != j.cfg.Name {
		return nats.StreamConfig{}, errors.New("nats: stub: unknown stream " + name)
	}
	return j.cfg, nil
}

// Publish on the stub is unreachable — the publisher path uses
// stubPublisher above, not this JetStream surface. We keep the method
// to satisfy nats.JetStream so tests can construct a Publisher off the
// stub if they need to.
func (j *stubJetStream) Publish(context.Context, string, string, []byte) error {
	return errStubPublisherUnwired
}

// stubUnpublishedSource returns no rows — the reconciler ticks idle
// until the Postgres-backed source lands (separate child issue).
type stubUnpublishedSource struct{}

func (stubUnpublishedSource) FetchUnpublished(context.Context, time.Time, int) ([]worker.UnpublishedRow, error) {
	return nil, nil
}

// stubWebhookHandler answers POST /webhooks/{channel}/{webhook_token}
// with a fixed 200 OK + empty JSON when the security_v2 feature flag is
// off. ADR §2 D5 anti-enumeration applies even to the stub: callers
// can't infer whether the real pipeline is enabled by probing it.
func stubWebhookHandler(logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		channel := r.PathValue("channel")
		if logger != nil {
			logger.Debug("webhook stub — flag off, dropping payload",
				slog.String("channel", channel),
			)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}
