// Package customdomainnet is a static analyzer that enforces ADR 0079 §1
// (SIN-62242) at compile time: no file under
// internal/customdomain/validation/... may import net/http (or any other
// package matching the configurable HTTPSubstr).
//
// The reason is structural: the validation use-case is a pure-domain
// hexagonal core. If any code path inside it could reach net/http, an
// attacker could redirect ownership-validation traffic, bypass the IP
// allowlist (which sits at the resolver port), or mount a SSRF via
// http.Get. We close the door at the import statement so the security
// review never has to chase a runtime call site.
//
// The analyzer is pattern-identical to internal/lint/aicache (ADR 0077
// §3.4) so reviewers see the same shape twice. Substring-based matching
// keeps it module-agnostic.
package customdomainnet

import (
	"flag"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Config tunes the analyzer. CRMSubstr identifies the protected package
// path; HTTPSubstr is the import we forbid. Defaults match the SIN/CRM
// layout (ADR 0079 §1).
type Config struct {
	// ProtectedSubstr identifies the bounded context that must stay
	// off net/http. Files whose package import path contains this
	// substring are policed.
	ProtectedSubstr string
	// ForbiddenImports lists exact import paths that are rejected. We
	// use exact-match here (not substring) because there are legitimate
	// stdlib imports whose names overlap (`net`, `net/netip`, `net/url`).
	ForbiddenImports []string
}

// DefaultConfig matches the SIN/CRM layout.
var DefaultConfig = Config{
	ProtectedSubstr:  "/internal/customdomain/validation",
	ForbiddenImports: []string{"net/http", "net/http/httptest", "net/http/httputil", "net/rpc"},
}

// NewAnalyzer returns a fresh analyzer wired to cfg. Tests pass an
// override that points at testdata stubs; production wiring uses
// Analyzer (the package-level default-config one).
func NewAnalyzer(cfg Config) *analysis.Analyzer {
	a := &analysis.Analyzer{
		Name: "customdomainnet",
		Doc: "customdomainnet forbids net/http imports under " +
			"internal/customdomain/validation/* (ADR 0079 §1).",
		URL: "https://pkg.go.dev/github.com/pericles-luz/crm/internal/lint/customdomainnet",
		Run: runWith(cfg),
	}
	fs := flag.NewFlagSet("customdomainnet", flag.ContinueOnError)
	fs.StringVar(&cfg.ProtectedSubstr, "protected-substr", cfg.ProtectedSubstr,
		"substring identifying the protected import path")
	a.Flags = *fs
	return a
}

// Analyzer is the default-config analyzer wired for production CI. It is
// referenced by cmd/customdomainnet (singlechecker) and from the
// golangci-lint custom rules block.
var Analyzer = NewAnalyzer(DefaultConfig)

func runWith(cfg Config) func(*analysis.Pass) (interface{}, error) {
	forbidden := map[string]struct{}{}
	for _, p := range cfg.ForbiddenImports {
		forbidden[p] = struct{}{}
	}
	return func(pass *analysis.Pass) (interface{}, error) {
		if !strings.Contains(pass.Pkg.Path(), cfg.ProtectedSubstr) {
			return nil, nil
		}
		for _, file := range pass.Files {
			for _, imp := range file.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				if _, hit := forbidden[path]; !hit {
					continue
				}
				pass.Report(analysis.Diagnostic{
					Pos: imp.Pos(),
					End: imp.End(),
					Message: "package " + pass.Pkg.Path() + " is under " +
						cfg.ProtectedSubstr + " but imports " + path +
						"; the validation use-case must stay net/http-free (ADR 0079 §1)",
				})
			}
		}
		return nil, nil
	}
}
