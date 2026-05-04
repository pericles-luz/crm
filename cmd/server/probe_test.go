package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/worker"
)

type fakeSource struct {
	rows []worker.UnpublishedRow
	err  error
}

func (f *fakeSource) FetchUnpublished(context.Context, time.Time, int) ([]worker.UnpublishedRow, error) {
	return f.rows, f.err
}

func TestProbedSource_RecordsLastFetchOnSuccess(t *testing.T) {
	t.Parallel()
	src := newProbedSource(&fakeSource{})
	if got := src.LastFetch(); !got.IsZero() {
		t.Fatalf("LastFetch before any tick = %v, want zero", got)
	}
	if _, err := src.FetchUnpublished(context.Background(), time.Now(), 10); err != nil {
		t.Fatalf("FetchUnpublished: %v", err)
	}
	if got := src.LastFetch(); got.IsZero() {
		t.Fatal("LastFetch after successful tick = zero")
	}
}

func TestProbedSource_DoesNotRecordOnError(t *testing.T) {
	t.Parallel()
	src := newProbedSource(&fakeSource{err: errors.New("boom")})
	_, _ = src.FetchUnpublished(context.Background(), time.Now(), 10)
	if got := src.LastFetch(); !got.IsZero() {
		t.Fatalf("LastFetch on error = %v, want zero", got)
	}
}

func TestHealthHandler_OKWhenNoProbe(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	healthHandlerFor(nil, time.Minute, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body = %v", body)
	}
}

func TestHealthHandler_StartingBeforeFirstTick(t *testing.T) {
	t.Parallel()
	src := newProbedSource(&fakeSource{})
	rec := httptest.NewRecorder()
	healthHandlerFor(src, time.Minute, func() time.Time { return time.Unix(1, 0) }).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["status"] != "starting" {
		t.Fatalf("status = %v, want starting", body["status"])
	}
}

func TestHealthHandler_DegradedWhenStale(t *testing.T) {
	t.Parallel()
	src := newProbedSource(&fakeSource{})
	if _, err := src.FetchUnpublished(context.Background(), time.Now(), 1); err != nil {
		t.Fatalf("FetchUnpublished: %v", err)
	}
	// Pretend "now" is 10 minutes after the recorded last fetch.
	now := func() time.Time { return time.Now().Add(10 * time.Minute) }
	rec := httptest.NewRecorder()
	healthHandlerFor(src, time.Minute, now).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["status"] != "degraded" {
		t.Fatalf("status = %v, want degraded", body["status"])
	}
}

func TestHealthHandler_OKWhenRecentlyTicked(t *testing.T) {
	t.Parallel()
	src := newProbedSource(&fakeSource{})
	if _, err := src.FetchUnpublished(context.Background(), time.Now(), 1); err != nil {
		t.Fatalf("FetchUnpublished: %v", err)
	}
	rec := httptest.NewRecorder()
	healthHandlerFor(src, time.Minute, time.Now).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
}

func TestFormatLast_ZeroReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := formatLast(time.Time{}); got != "" {
		t.Fatalf("formatLast(zero) = %q", got)
	}
	now := time.Now()
	if got := formatLast(now); got == "" {
		t.Fatalf("formatLast(now) = empty")
	}
}
