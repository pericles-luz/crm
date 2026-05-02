// Package inmem is an in-memory ReconcileQueue used by tests and by
// the dev compose stack while the NATS adapter is in flight (separate
// task SIN-62193). The implementation is a buffered channel.
package inmem

import (
	"context"
	"errors"

	"github.com/pericles-luz/crm/internal/wallet/port"
)

// ErrFull is returned when Enqueue is called against a saturated queue.
var ErrFull = errors.New("inmem queue: full")

// Queue is the bounded in-memory reconcile queue.
type Queue struct {
	ch chan port.ReconcileJob
}

// New creates a queue with the given capacity (defaults to 1024 if <=0).
func New(capacity int) *Queue {
	if capacity <= 0 {
		capacity = 1024
	}
	return &Queue{ch: make(chan port.ReconcileJob, capacity)}
}

// Enqueue pushes a job; non-blocking. Returns ErrFull if the buffer is
// saturated so the caller (CommitDebit) can decide between retrying
// or surfacing the error to the API.
func (q *Queue) Enqueue(ctx context.Context, job port.ReconcileJob) error {
	select {
	case q.ch <- job:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrFull
	}
}

// Dequeue blocks waiting for a job or ctx cancellation.
func (q *Queue) Dequeue(ctx context.Context) (port.ReconcileJob, error) {
	select {
	case j := <-q.ch:
		return j, nil
	case <-ctx.Done():
		return port.ReconcileJob{}, ctx.Err()
	}
}

// Len returns the current depth, used by tests.
func (q *Queue) Len() int { return len(q.ch) }
