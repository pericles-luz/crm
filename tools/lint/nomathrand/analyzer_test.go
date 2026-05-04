package nomathrand_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/pericles-luz/crm/tools/lint/nomathrand"
)

// fixtureConfig points the analyzer at the testdata stubs. Substring
// match means the fixture's `internal/webhook` import path stands in
// for the real `internal/webhook` of the SIN/CRM repo.
var fixtureConfig = nomathrand.Config{
	WebhookSubstr: "/internal/webhook",
	GenFileSuffix: "gen.go",
}

// TestAnalyzer_FlagsWebhookImport exercises rule 1: a math/rand import
// in a file under the webhook package is flagged. The fixture also
// includes a `// nomathrand:ok` suppression that must NOT trigger.
func TestAnalyzer_FlagsWebhookImport(t *testing.T) {
	a := nomathrand.NewAnalyzer(fixtureConfig)
	analysistest.Run(t, analysistest.TestData(), a,
		"github.com/pericles-luz/crm/internal/webhook",
	)
}

// TestAnalyzer_FlagsGenFileImport exercises rule 2: a math/rand import
// in a file ending in `gen.go` is flagged regardless of package path.
func TestAnalyzer_FlagsGenFileImport(t *testing.T) {
	a := nomathrand.NewAnalyzer(fixtureConfig)
	analysistest.Run(t, analysistest.TestData(), a, "genfilepkg")
}

// TestAnalyzer_AllowsOutOfScope exercises the negative case: a
// math/rand import in a non-webhook, non-gen.go file does NOT trigger.
func TestAnalyzer_AllowsOutOfScope(t *testing.T) {
	a := nomathrand.NewAnalyzer(fixtureConfig)
	analysistest.Run(t, analysistest.TestData(), a, "safepkg")
}

// TestAnalyzer_AllowsCryptoRand exercises the positive default: a
// crypto/rand import in a webhook-scoped fixture is allowed.
func TestAnalyzer_AllowsCryptoRand(t *testing.T) {
	a := nomathrand.NewAnalyzer(fixtureConfig)
	analysistest.Run(t, analysistest.TestData(), a, "webhookpkg")
}

// TestAnalyzer_RejectsBareSuppressionMarker proves a `// nomathrand:ok`
// without a reason does NOT silence the rule. The audit trail is the
// reason this lint exists; bare suppressions defeat it.
func TestAnalyzer_RejectsBareSuppressionMarker(t *testing.T) {
	a := nomathrand.NewAnalyzer(fixtureConfig)
	analysistest.Run(t, analysistest.TestData(), a, "emptyreasonpkg")
}

