package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/pericles-luz/crm/internal/worker"
)

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
