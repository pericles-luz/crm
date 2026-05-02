package port

import "context"

// ReconcileJob is the payload pushed onto the reconciliation queue.
type ReconcileJob struct {
	EntryID  string
	WalletID string
	Attempts int
}

// ReconcileQueue is the bus used to hand off pending entries that the
// inline retry loop could not commit. A worker drains it asynchronously.
type ReconcileQueue interface {
	Enqueue(ctx context.Context, job ReconcileJob) error
	// Dequeue blocks until a job is available or ctx is cancelled.
	// Returns ctx.Err() on cancellation. Implementations should be safe
	// for use by a single consumer goroutine.
	Dequeue(ctx context.Context) (ReconcileJob, error)
}
