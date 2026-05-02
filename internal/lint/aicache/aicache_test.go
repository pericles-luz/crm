package aicache_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/pericles-luz/crm/internal/lint/aicache"
)

// fixtureConfig points the analyzer at the testdata stubs. Substrings match
// the fake GOPATH layout under testdata/src/.
var fixtureConfig = aicache.Config{
	AISubstr:       "/internal/ai/",
	AdapterSubstr:  "/internal/ai/adapter/redis",
	RedisPkgSubstr: "github.com/redis/go-redis/",
	CachePkgSubstr: "/internal/ai/cache",
}

func TestAnalyzer_FlagsRedisImportOutsideAdapter(t *testing.T) {
	a := aicache.NewAnalyzer(fixtureConfig)
	analysistest.Run(t, analysistest.TestData(), a,
		"github.com/pericles-luz/crm/internal/ai/usecase_bad",
	)
}

func TestAnalyzer_AcceptsCleanPackage(t *testing.T) {
	a := aicache.NewAnalyzer(fixtureConfig)
	analysistest.Run(t, analysistest.TestData(), a,
		"github.com/pericles-luz/crm/internal/ai/usecase_good",
	)
}

func TestAnalyzer_AdapterPositiveAndNegative(t *testing.T) {
	a := aicache.NewAnalyzer(fixtureConfig)
	analysistest.Run(t, analysistest.TestData(), a,
		"github.com/pericles-luz/crm/internal/ai/adapter/redis",
	)
}

// TestAnalyzer_SkipsPackagesOutsideAI confirms that the analyzer never
// reports diagnostics for packages whose path does not contain the AI
// bounded-context substring. We point it at a path-mismatched package: the
// stub redis package itself, which happens to contain redis.Client.Get
// definitions but is not under internal/ai/.
func TestAnalyzer_SkipsPackagesOutsideAI(t *testing.T) {
	a := aicache.NewAnalyzer(fixtureConfig)
	analysistest.Run(t, analysistest.TestData(), a,
		"github.com/redis/go-redis/v9",
	)
}
