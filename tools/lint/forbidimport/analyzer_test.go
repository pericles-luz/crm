package forbidimport_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/pericles-luz/crm/tools/lint/forbidimport"
)

// TestAnalyzer_FlagsForbiddenImports verifies the happy regression case:
// every forbidden import in badpkg has a `// want "<diag>"` comment.
func TestAnalyzer_FlagsForbiddenImports(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), forbidimport.Analyzer, "badpkg")
}

// TestAnalyzer_AllowsCleanPackage proves the analyzer does not over-fire
// on a domain package that imports nothing forbidden.
func TestAnalyzer_AllowsCleanPackage(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), forbidimport.Analyzer, "goodpkg")
}

// TestAnalyzer_AllowsAdapterPackage proves the path-allowlist works: any
// package whose import path is rooted at internal/adapter/db/postgres is
// allowed to import the SQL driver directly because it IS the seam.
func TestAnalyzer_AllowsAdapterPackage(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), forbidimport.Analyzer,
		"github.com/pericles-luz/crm/internal/adapter/db/postgres/inadapter")
}

// TestAnalyzer_AllowsExternalAdapterTestPackage proves the `_test` suffix
// stripping works: Go reports external test files (`package foo_test`) as
// import path `<foo>_test`, and the adapter's external test package must
// share the allowlist with the adapter itself.
func TestAnalyzer_AllowsExternalAdapterTestPackage(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), forbidimport.Analyzer,
		"github.com/pericles-luz/crm/internal/adapter/db/postgres_test")
}

// TestAnalyzer_HonorsAnnotatedOverride verifies that a // forbidimport:ok
// marker with a justification silences the rule for one import line.
func TestAnalyzer_HonorsAnnotatedOverride(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), forbidimport.Analyzer, "overridepkg")
}

// TestAnalyzer_RejectsBareOverride verifies that a bare // forbidimport:ok
// (no justification) does NOT silence the rule. Bare markers slip through
// review unnoticed.
func TestAnalyzer_RejectsBareOverride(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), forbidimport.Analyzer, "bareoverride")
}
