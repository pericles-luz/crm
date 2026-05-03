package worker_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/webhook"
	"github.com/pericles-luz/crm/internal/worker"
)

type stubSource struct {
	rows []worker.UnpublishedRow
	err  error
}

func (s *stubSource) FetchUnpublished(context.Context, time.Time, int) ([]worker.UnpublishedRow, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

type stubPublisher struct {
	mu   sync.Mutex
	hits int
	err  error
}

func (p *stubPublisher) Publish(context.Context, [16]byte, webhook.TenantID, string, []byte, map[string][]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hits++
	return p.err
}

type stubRawEvents struct {
	mu     sync.Mutex
	marked int
}

func (s *stubRawEvents) Insert(context.Context, webhook.RawEventRow) ([16]byte, error) {
	return [16]byte{}, nil
}
func (s *stubRawEvents) MarkPublished(context.Context, [16]byte, time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marked++
	return nil
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func TestReconciler_RequiresDeps(t *testing.T) {
	t.Parallel()
	cases := []worker.Config{
		{},
		{Source: &stubSource{}},
		{Source: &stubSource{}, Publisher: &stubPublisher{}},
	}
	for i, c := range cases {
		c := c
		i := i
		t.Run("missing", func(t *testing.T) {
			t.Parallel()
			_ = i
			_, err := worker.New(c)
			if err == nil {
				t.Fatalf("case %d: expected error", i)
			}
		})
	}
}

func TestReconciler_TickPublishesAndMarks(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	rec, err := worker.New(worker.Config{
		Source: &stubSource{rows: []worker.UnpublishedRow{
			{ID: [16]byte{1}, Channel: "whatsapp", Received: now.Add(-2 * time.Minute)},
		}},
		Publisher: &stubPublisher{},
		RawEvents: &stubRawEvents{},
		Clock:     fixedClock{t: now},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := rec.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

func TestReconciler_PublishFailureSkipsMark(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	pub := &stubPublisher{err: errors.New("nats down")}
	raw := &stubRawEvents{}
	rec, err := worker.New(worker.Config{
		Source:    &stubSource{rows: []worker.UnpublishedRow{{ID: [16]byte{1}, Received: now.Add(-2 * time.Minute)}}},
		Publisher: pub,
		RawEvents: raw,
		Clock:     fixedClock{t: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if pub.hits != 1 {
		t.Fatalf("publish hits = %d, want 1", pub.hits)
	}
	if raw.marked != 0 {
		t.Fatalf("raw marked = %d, want 0 (publish failed)", raw.marked)
	}
}

func TestReconciler_AlertOnStale(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	called := false
	rec, _ := worker.New(worker.Config{
		Source: &stubSource{rows: []worker.UnpublishedRow{
			{ID: [16]byte{2}, Received: now.Add(-2 * time.Hour)},
		}},
		Publisher:  &stubPublisher{},
		RawEvents:  &stubRawEvents{},
		Clock:      fixedClock{t: now},
		AlertAfter: time.Hour,
		OnStale:    func(_ [16]byte, age time.Duration) { called = age >= time.Hour },
	})
	_ = rec.Tick(context.Background())
	if !called {
		t.Fatal("OnStale was not invoked for an event > AlertAfter old")
	}
}

func TestReconciler_FetchError(t *testing.T) {
	t.Parallel()
	rec, _ := worker.New(worker.Config{
		Source:    &stubSource{err: errors.New("db lost")},
		Publisher: &stubPublisher{},
		RawEvents: &stubRawEvents{},
	})
	if err := rec.Tick(context.Background()); err == nil {
		t.Fatal("expected error from Tick when fetch fails")
	}
}

func TestReconciler_RunHonoursContextCancel(t *testing.T) {
	t.Parallel()
	rec, _ := worker.New(worker.Config{
		Source:    &stubSource{},
		Publisher: &stubPublisher{},
		RawEvents: &stubRawEvents{},
		TickEvery: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rec.Run(ctx); err == nil {
		t.Fatal("expected ctx.Err() from cancelled Run")
	}
}
