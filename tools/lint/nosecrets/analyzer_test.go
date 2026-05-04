package nosecrets_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/pericles-luz/crm/tools/lint/nosecrets"
)

// fixtureConfig points the analyzer at the testdata stubs. Substrings match
// the fake GOPATH layout under testdata/src/.
var fixtureConfig = nosecrets.Config{
	WebhookSubstr:       "/internal/webhook",
	AdapterSubstr:       "/internal/adapter",
	WebhookImportSubstr: "/internal/webhook",
	PreHMACGate:         "VerifyApp",
}

// TestAnalyzer_FlagsPreHMACAndAlwaysSecrets exercises the webhook-package
// handler fixture: tenant_id pre-HMAC, raw_payload, webhook_token, and
// Authorization all flagged; post-HMAC tenant_id and the override marker
// stay silent.
func TestAnalyzer_FlagsPreHMACAndAlwaysSecrets(t *testing.T) {
	a := nosecrets.NewAnalyzer(fixtureConfig)
	analysistest.Run(t, analysistest.TestData(), a,
		"github.com/pericles-luz/crm/internal/webhook/handler",
	)
}

// TestAnalyzer_FlagsAdapterImportingWebhook exercises an adapter package
// that imports webhook: always-forbidden secrets fire, pre-HMAC tenant_slug
// fires, and *slog.Logger receiver methods are also policed.
func TestAnalyzer_FlagsAdapterImportingWebhook(t *testing.T) {
	a := nosecrets.NewAnalyzer(fixtureConfig)
	analysistest.Run(t, analysistest.TestData(), a,
		"github.com/pericles-luz/crm/internal/adapter/transport",
	)
}

// TestAnalyzer_AcceptsAdapterWithoutWebhookImport exercises an adapter that
// does NOT import webhook. tenant_id / tenant_slug are allowed because the
// adapter is not on the webhook path; no diagnostics expected.
func TestAnalyzer_AcceptsAdapterWithoutWebhookImport(t *testing.T) {
	a := nosecrets.NewAnalyzer(fixtureConfig)
	analysistest.Run(t, analysistest.TestData(), a,
		"github.com/pericles-luz/crm/internal/adapter/clean",
	)
}

// TestAnalyzer_SkipsPackagesOutsideScope confirms the analyzer ignores
// packages whose path matches neither webhook nor adapter substrings.
func TestAnalyzer_SkipsPackagesOutsideScope(t *testing.T) {
	a := nosecrets.NewAnalyzer(fixtureConfig)
	analysistest.Run(t, analysistest.TestData(), a, "goodpkg")
}
