package nobodyreread_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/pericles-luz/crm/tools/lint/nobodyreread"
)

// TestAnalyzer_FlagsMultiReads exercises every double-read pattern. Each
// flagged call must have a `// want "<diag>"` comment in the fixture.
func TestAnalyzer_FlagsMultiReads(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), nobodyreread.Analyzer, "badpkg")
}

// TestAnalyzer_AllowsSingleReadAndOverride proves the lint stays silent on
// the canonical webhook-handler shape (single read with MaxBytesReader),
// MaxBytesReader-only wraps, Body.Close, override markers, and reads scoped
// to nested function literals.
func TestAnalyzer_AllowsSingleReadAndOverride(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), nobodyreread.Analyzer, "goodpkg")
}

// TestAnalyzer_FlagsMiddlewareBodyConsumption checks that a function with the
// `func(http.Handler) http.Handler` signature is flagged for ANY body read,
// while a sibling middleware that only wraps with MaxBytesReader (no read)
// stays silent.
func TestAnalyzer_FlagsMiddlewareBodyConsumption(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), nobodyreread.Analyzer, "middlewarepkg")
}
