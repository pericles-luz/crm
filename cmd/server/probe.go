package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/pericles-luz/crm/internal/worker"
)

// probedSource wraps a worker.UnpublishedSource and records the time of
// the last successful fetch. The readiness probe consults LastFetch to
// decide whether the reconciler is alive (ADR §6 — health reflects
// reconciler liveness so an outage is visible in load-balancer probes).
type probedSource struct {
	inner       worker.UnpublishedSource
	lastFetchNs atomic.Int64
}

func newProbedSource(inner worker.UnpublishedSource) *probedSource {
	return &probedSource{inner: inner}
}

func (s *probedSource) FetchUnpublished(ctx context.Context, olderThan time.Time, limit int) ([]worker.UnpublishedRow, error) {
	rows, err := s.inner.FetchUnpublished(ctx, olderThan, limit)
	if err == nil {
		s.lastFetchNs.Store(time.Now().UnixNano())
	}
	return rows, err
}

// LastFetch returns the time of the last successful fetch, or the zero
// Time if no fetch has succeeded yet.
func (s *probedSource) LastFetch() time.Time {
	ns := s.lastFetchNs.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// healthHandlerFor returns an HTTP handler whose status reflects the
// reconciler's liveness. When probe is nil (feature flag off, no
// reconciler), the handler always answers 200 OK.
func healthHandlerFor(probe *probedSource, staleness time.Duration, now func() time.Time) http.Handler {
	if now == nil {
		now = time.Now
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{"status": "ok"}
		code := http.StatusOK
		if probe != nil {
			last := probe.LastFetch()
			body["reconciler"] = map[string]any{
				"last_fetch":     formatLast(last),
				"staleness_max":  staleness.String(),
			}
			if last.IsZero() {
				body["status"] = "starting"
				body["reason"] = "reconciler has not completed a tick yet"
				code = http.StatusServiceUnavailable
			} else if now().Sub(last) > staleness {
				body["status"] = "degraded"
				body["reason"] = "reconciler tick is stale"
				code = http.StatusServiceUnavailable
			}
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	})
}

func formatLast(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
