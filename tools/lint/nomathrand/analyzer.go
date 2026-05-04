// Package nomathrand implements a go/analysis pass that fails CI when a
// file under the webhook package, or any file ending in `gen.go` (the
// project convention for entropy/credential mints), imports
// `math/rand`. Token material in `webhook_tokens` MUST be sourced from
// `crypto/rand` (ADR 0075 §2 D1 / F-10); a regression here silently
// reduces token entropy from CSPRNG to a 64-bit mt19937 stream.
//
// Two rules:
//
//  1. ANY file whose package path matches the configured webhook
//     substring (default `/internal/webhook`) must not import
//     `math/rand` (or `math/rand/v2`).
//  2. ANY file whose base name ends in `gen.go` (any package) must not
//     import `math/rand` either. The base-name rule covers helpers
//     placed outside `internal/webhook/` that still mint credentials.
//
// Override either rule for a specific import line with a
// `// nomathrand:ok <reason>` comment on (or immediately above) the
// import spec.
//
// Wire it up in CI with the paperclip-lint multichecker (preferred):
//
//	paperclip-lint check ./...
//
// or as a stand-alone vet tool:
//
//	go vet -vettool=$(which paperclip-lint) ./...
package nomathrand

import (
	"flag"
	"go/ast"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Config bundles the substrings and suffix the analyzer matches on.
// Substring matching keeps the analyzer module-agnostic: changing the
// project's import path does not require touching the rule.
type Config struct {
	// WebhookSubstr identifies the webhook package by package path.
	// Files whose package path contains this substring are policed.
	// Default: "/internal/webhook".
	WebhookSubstr string
	// GenFileSuffix is the trailing filename pattern that triggers the
	// rule regardless of package. Default: "gen.go".
	GenFileSuffix string
}

// DefaultConfig matches the SIN/CRM layout per ADR 0075.
var DefaultConfig = Config{
	WebhookSubstr: "/internal/webhook",
	GenFileSuffix: "gen.go",
}

const suppressMarker = "nomathrand:ok"

// forbiddenImports enumerates the import paths that may not appear in a
// policed file. Both v1 and v2 of math/rand are caught — v2's interface
// improved but the underlying entropy source is still mt19937.
var forbiddenImports = []string{
	"math/rand",
	"math/rand/v2",
}

// NewAnalyzer returns a fresh analyzer wired to cfg. Tests use this
// constructor to point the analyzer at testdata stubs with a different
// substring/suffix.
func NewAnalyzer(cfg Config) *analysis.Analyzer {
	a := &analysis.Analyzer{
		Name: "nomathrand",
		Doc:  "reports `math/rand` imports under internal/webhook/ and in *gen.go files (ADR 0075 §2 D1 / F-10).",
		URL:  "https://pkg.go.dev/github.com/pericles-luz/crm/tools/lint/nomathrand",
		Run:  runWith(cfg),
	}
	fs := flag.NewFlagSet("nomathrand", flag.ContinueOnError)
	fs.StringVar(&cfg.WebhookSubstr, "webhook-substr", cfg.WebhookSubstr, "substring identifying the webhook package path (always policed)")
	fs.StringVar(&cfg.GenFileSuffix, "gen-suffix", cfg.GenFileSuffix, "filename suffix that triggers the rule regardless of package (default `gen.go`)")
	a.Flags = *fs
	return a
}

// Analyzer is the default-config analyzer wired for production CI.
var Analyzer = NewAnalyzer(DefaultConfig)

func runWith(cfg Config) func(*analysis.Pass) (interface{}, error) {
	return func(pass *analysis.Pass) (interface{}, error) {
		pkgPath := pass.Pkg.Path()
		inWebhook := cfg.WebhookSubstr != "" && strings.Contains(pkgPath, cfg.WebhookSubstr)

		for _, file := range pass.Files {
			pos := pass.Fset.Position(file.Pos())
			isGenFile := cfg.GenFileSuffix != "" && hasGenSuffix(pos.Filename, cfg.GenFileSuffix)
			if !inWebhook && !isGenFile {
				continue
			}
			suppressed := suppressionLines(pass.Fset, file)
			for _, imp := range file.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					continue
				}
				if !isForbidden(path) {
					continue
				}
				line := pass.Fset.Position(imp.Pos()).Line
				if suppressed[line] {
					continue
				}
				scope := scopeLabel(inWebhook, isGenFile)
				pass.Reportf(imp.Pos(),
					"file imports %q in %s; webhook token material MUST come from crypto/rand (ADR 0075 §2 D1 / F-10) — switch to crypto/rand or add // %s <reason>",
					path, scope, suppressMarker)
			}
		}
		return nil, nil
	}
}

// scopeLabel makes diagnostics tell the developer which rule fired so
// they can fix the right thing (move the file vs swap the import).
func scopeLabel(inWebhook, isGenFile bool) string {
	switch {
	case inWebhook && isGenFile:
		return "webhook + *gen.go scope"
	case inWebhook:
		return "webhook scope"
	default:
		return "*gen.go scope"
	}
}

// hasGenSuffix matches files whose basename ends with suffix and is
// preceded by an underscore, a dot, or path-component start (so
// `gen.go` matches `gen.go`, `tokenmint_gen.go`, `pkg.gen.go` but not
// `regen.go` or `gen_other.go`). The convention this lints against is
// "files that mint things", which by team rule end in `_gen.go`,
// `.gen.go`, or are simply `gen.go`.
func hasGenSuffix(filename, suffix string) bool {
	base := filepath.Base(filename)
	if base == suffix {
		return true
	}
	if !strings.HasSuffix(base, suffix) {
		return false
	}
	prefixLen := len(base) - len(suffix)
	if prefixLen == 0 {
		return true
	}
	c := base[prefixLen-1]
	return c == '_' || c == '.'
}

// isForbidden reports whether path is one of the forbidden imports
// (case-sensitive, exact match). Sub-packages of math/rand are also
// caught because they share the same entropy contract.
func isForbidden(path string) bool {
	for _, f := range forbiddenImports {
		if path == f || strings.HasPrefix(path, f+"/") {
			return true
		}
	}
	return false
}

// suppressionLines returns the set of source-file line numbers carrying
// a `// nomathrand:ok <reason>` marker. A diagnostic on the SAME line,
// or on the line immediately following the marker, is silenced.
//
// Markers MUST carry a non-empty reason after the suppress token. Bare
// `// nomathrand:ok` markers do NOT silence the rule — empty
// suppressions defeat the audit trail this lint exists to keep.
func suppressionLines(fset *token.FileSet, f *ast.File) map[int]bool {
	out := make(map[int]bool)
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
			text = strings.TrimSpace(strings.TrimPrefix(text, "/*"))
			text = strings.TrimSpace(strings.TrimSuffix(text, "*/"))
			if !strings.HasPrefix(text, suppressMarker) {
				continue
			}
			rest := strings.TrimSpace(strings.TrimPrefix(text, suppressMarker))
			if rest == "" {
				continue
			}
			line := fset.Position(c.Pos()).Line
			out[line] = true
			out[line+1] = true
		}
	}
	return out
}
