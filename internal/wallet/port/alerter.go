package port

import "context"

// Alert is a payload pushed to #alerts (Slack or otherwise).
type Alert struct {
	// Code is the alert identifier, e.g. "wallet.reconciliation_drift".
	Code string
	// Subject is a one-line summary suitable as a Slack title.
	Subject string
	// Detail is human-readable detail (markdown OK).
	Detail string
	// Fields are structured key/values surfaced as Slack attachments.
	Fields map[string]string
}

// Alerter is the port the reconciliator and worker use to escalate
// drift / exhausted retries. The Slack adapter lives elsewhere; in
// tests we use a recording fake.
type Alerter interface {
	Send(ctx context.Context, a Alert) error
}
