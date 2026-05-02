package inmem_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/wallet/adapter/queue/inmem"
	"github.com/pericles-luz/crm/internal/wallet/port"
)

func TestQueue_EnqueueDequeue(t *testing.T) {
	q := inmem.New(2)
	job := port.ReconcileJob{EntryID: "e", WalletID: "w"}
	if err := q.Enqueue(context.Background(), job); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if q.Len() != 1 {
		t.Fatalf("len: %d", q.Len())
	}
	got, err := q.Dequeue(context.Background())
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if got != job {
		t.Fatalf("got %+v", got)
	}
}

func TestQueue_FullAndCtxCancel(t *testing.T) {
	q := inmem.New(1)
	if err := q.Enqueue(context.Background(), port.ReconcileJob{EntryID: "1"}); err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	if err := q.Enqueue(context.Background(), port.ReconcileJob{EntryID: "2"}); !errors.Is(err, inmem.ErrFull) {
		t.Fatalf("full: got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.Enqueue(ctx, port.ReconcileJob{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx cancel: got %v", err)
	}
	// Drain once so dequeue can be tested with cancel.
	_, _ = q.Dequeue(context.Background())
	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer dcancel()
	_, err := q.Dequeue(dctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dequeue timeout: got %v", err)
	}
}

func TestQueue_DefaultCapacity(t *testing.T) {
	q := inmem.New(0) // → 1024
	for i := 0; i < 100; i++ {
		if err := q.Enqueue(context.Background(), port.ReconcileJob{EntryID: "x"}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
}
