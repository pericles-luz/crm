package port

import (
	"context"
	"time"
)

// OpenRouterCostSample is the daily token usage reported by the
// OpenRouter cost API for a master.
type OpenRouterCostSample struct {
	MasterID string
	Date     time.Time // start of day in UTC
	// Tokens is the canonical unit our wallet uses; the adapter is
	// responsible for converting whatever OpenRouter reports.
	Tokens int64
}

// OpenRouterCostAPI is the read port the daily drift batch consumes.
// The full adapter (real HTTP client) is documented as a child issue;
// this PR ships the port + a recording fake used by tests.
type OpenRouterCostAPI interface {
	DailyUsage(ctx context.Context, masterID string, day time.Time) (OpenRouterCostSample, error)
}

// IDGenerator generates stable IDs for new ledger entries. Centralised
// so tests can plug a deterministic generator without sprinkling
// time-based ID calls in domain code.
type IDGenerator interface {
	NewID() string
}
