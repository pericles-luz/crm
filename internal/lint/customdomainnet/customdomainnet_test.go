package customdomainnet_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/pericles-luz/crm/internal/lint/customdomainnet"
)

// fixtureConfig points the analyzer at the testdata stubs. Substrings
// match the fake GOPATH layout under testdata/src/.
var fixtureConfig = customdomainnet.Config{
	ProtectedSubstr:  "/internal/customdomain/validation",
	ForbiddenImports: []string{"net/http"},
}

func TestAnalyzer_FlagsHTTPImportInsideProtectedTree(t *testing.T) {
	a := customdomainnet.NewAnalyzer(fixtureConfig)
	analysistest.Run(t, analysistest.TestData(), a,
		"github.com/pericles-luz/crm/internal/customdomain/validation/bad",
	)
}

func TestAnalyzer_AcceptsCleanPackageInsideProtectedTree(t *testing.T) {
	a := customdomainnet.NewAnalyzer(fixtureConfig)
	analysistest.Run(t, analysistest.TestData(), a,
		"github.com/pericles-luz/crm/internal/customdomain/validation/good",
	)
}

func TestAnalyzer_SkipsPackagesOutsideProtectedTree(t *testing.T) {
	a := customdomainnet.NewAnalyzer(fixtureConfig)
	analysistest.Run(t, analysistest.TestData(), a,
		"github.com/pericles-luz/crm/internal/other",
	)
}
