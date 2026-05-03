package notenant_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/pericles-luz/crm/tools/lint/notenant"
)

// TestAnalyzer_FlagsPoolMethods runs the analyzer against the testdata
// fixture and verifies every flagged call has a `// want "<diag>"` comment.
//
// analysistest sets GOPATH=testdata so the testdata/src/<importpath>/<file>
// layout gives us a working stand-in for pgxpool without a real go.mod
// dependency. See tools/lint/notenant/testdata/src/github.com/jackc/pgx/v5/
// pgxpool/pool.go.
func TestAnalyzer_FlagsPoolMethods(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), notenant.Analyzer, "badpkg")
}

// TestAnalyzer_AllowsTxAndOtherTypes makes sure the analyzer does not
// over-fire: pgx.Tx (what WithTenant hands callers) and any other type
// happening to expose Exec/Query/QueryRow are left alone.
func TestAnalyzer_AllowsTxAndOtherTypes(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), notenant.Analyzer, "goodpkg")
}

// TestAnalyzer_AllowsAdapterPackage proves the path-allowlist works: the
// adapter package IS where we wrap pgxpool, so it must be allowed to call
// pgxpool methods directly. Fixture lives under
// testdata/src/github.com/pericles-luz/crm/internal/adapter/db/postgres/...
// to match the real import path the analyzer tests against.
func TestAnalyzer_AllowsAdapterPackage(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), notenant.Analyzer,
		"github.com/pericles-luz/crm/internal/adapter/db/postgres/inadapter")
}
