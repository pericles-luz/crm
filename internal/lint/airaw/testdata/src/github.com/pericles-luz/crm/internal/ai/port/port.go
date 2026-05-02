// Package port is a minimal stand-in for the real internal/ai/port package,
// used only by the airaw analyzer's analysistest fixtures.
package port

import (
	"context"

	"github.com/pericles-luz/crm/internal/ai/domain"
)

type Summarizer interface {
	Summarize(ctx context.Context, conversationID string) (domain.AISummary, error)
}
