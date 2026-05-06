package port_test

import (
	"errors"
	"testing"

	aiport "github.com/pericles-luz/crm/internal/ai/port"
)

func TestErrLimiterUnavailable_Identity(t *testing.T) {
	t.Parallel()

	wrapped := errors.Join(errors.New("redis: dial tcp: connect refused"), aiport.ErrLimiterUnavailable)
	if !errors.Is(wrapped, aiport.ErrLimiterUnavailable) {
		t.Fatalf("errors.Is(wrapped, ErrLimiterUnavailable) = false, want true")
	}

	if aiport.ErrLimiterUnavailable.Error() == "" {
		t.Fatalf("ErrLimiterUnavailable.Error() is empty")
	}
}
