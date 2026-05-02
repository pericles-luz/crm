package port

// CommitOutcome labels the outcome of a single commit attempt for the
// wallet_commit_retry_total{outcome=...} counter.
type CommitOutcome string

const (
	OutcomeSuccess   CommitOutcome = "success"
	OutcomeRetry     CommitOutcome = "retry"
	OutcomeEnqueued  CommitOutcome = "enqueued"
	OutcomeExhausted CommitOutcome = "exhausted"
)

// Metrics is the observability port. Adapters wire to Prometheus or
// noop in tests/staging-without-prom.
//
// Counter / gauge methods MUST be cheap and safe for concurrent use.
type Metrics interface {
	// IncCommitRetry increments wallet_commit_retry_total{outcome}.
	IncCommitRetry(outcome CommitOutcome)
	// SetPendingEntries sets wallet_pending_entries_gauge.
	SetPendingEntries(n int64)
	// SetReconciliationDriftPct sets wallet_reconciliation_drift_pct{wallet_id}.
	SetReconciliationDriftPct(walletID string, pct float64)
	// SetOpenRouterDriftPct sets wallet_openrouter_drift_pct{master_id}.
	SetOpenRouterDriftPct(masterID string, pct float64)
}
