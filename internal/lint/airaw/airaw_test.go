package airaw_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/pericles-luz/crm/internal/lint/airaw"
)

func TestAnalyzer_Banned(t *testing.T) {
	dir := analysistest.TestData()
	analysistest.Run(t, dir, airaw.Analyzer,
		"github.com/pericles-luz/crm/internal/lint/airaw/fixtures/banned")
}

func TestAnalyzer_Allowed(t *testing.T) {
	dir := analysistest.TestData()
	analysistest.Run(t, dir, airaw.Analyzer,
		"github.com/pericles-luz/crm/internal/lint/airaw/fixtures/allowed")
}
