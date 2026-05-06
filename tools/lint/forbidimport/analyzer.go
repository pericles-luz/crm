// Package forbidimport implements a go/analysis pass that fails CI when any
// package outside the postgres adapter packages imports a SQL driver. The
// hexagonal boundary requires every database call to flow through the
// postgres adapter — domain code, use-cases, and HTTP handlers must depend
// on ports, never directly on database/sql or pgx. The adapter is split
// across two sibling sub-packages (internal/adapter/db/postgres for the
// pool/tenant/connection seam and internal/adapter/store/postgres for
// store implementations); both are allowlisted. See SIN-62216 and
// docs/architecture/import-rules.md.
package forbidimport

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Analyzer is the go/analysis pass that backs the `forbidimport` linter.
// Wire it up in CI with:
//
//	go vet -vettool=$(which forbidimport) ./internal/...
var Analyzer = &analysis.Analyzer{
	Name: "forbidimport",
	Doc:  "reports imports of database/sql or pgx/lib/pq outside the postgres adapter packages (internal/adapter/db/postgres and internal/adapter/store/postgres); route DB access through the postgres adapter (SIN-62216)",
	Run:  run,
}

// forbiddenExact lists import paths that are forbidden under the rule and
// matched with == semantics.
var forbiddenExact = map[string]struct{}{
	"database/sql":        {},
	"database/sql/driver": {},
	"github.com/lib/pq":   {},
}

// forbiddenPrefixes lists import-path prefixes treated as forbidden. A
// prefix match must align on a slash boundary (or full equality) so that
// "github.com/jackc/pgxpoolio" would not match the "github.com/jackc/pgx"
// prefix accidentally.
var forbiddenPrefixes = []string{
	"github.com/jackc/pgx",
	"github.com/jackc/pgconn",
}

// allowedPkgPrefixes are package-path prefixes whose Go files may import
// the forbidden packages directly. The postgres adapter is the seam
// between the domain and the SQL driver — exactly the place hexagonal
// architecture allows database/sql. The adapter is organized in two
// sibling sub-packages, both allowlisted:
//   - internal/adapter/db/postgres: pool, tenant scoping, testpg harness.
//   - internal/adapter/store/postgres: per-port store implementations
//     (idempotency, raw events, tenant association, webhook tokens, …)
//     that consume the pool above and expose clean domain ports upward.
var allowedPkgPrefixes = []string{
	"github.com/pericles-luz/crm/internal/adapter/db/postgres",
	"github.com/pericles-luz/crm/internal/adapter/store/postgres",
}

// overrideMarker is the magic comment that silences a single forbidden
// import. Bare markers without a justification are ignored, matching the
// nomathrand convention so override use is reviewed in PR.
const overrideMarker = "forbidimport:ok"

func run(pass *analysis.Pass) (any, error) {
	if isAllowedPkg(pass.Pkg.Path()) {
		return nil, nil
	}
	for _, file := range pass.Files {
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if !isForbidden(path) {
				continue
			}
			if hasOverride(pass.Fset, file, imp) {
				continue
			}
			pass.Reportf(imp.Pos(),
				"forbidden import %q outside the postgres adapter packages (internal/adapter/db/postgres, internal/adapter/store/postgres); route DB access through the postgres adapter (SIN-62216)",
				path)
		}
	}
	return nil, nil
}

func isForbidden(path string) bool {
	if _, ok := forbiddenExact[path]; ok {
		return true
	}
	for _, p := range forbiddenPrefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

func isAllowedPkg(pkgPath string) bool {
	// External test files declare `package foo_test`; Go reports their
	// package path as `<importpath>_test`. Strip that suffix so the
	// adapter's external test package shares the allowlist with the
	// adapter itself.
	pkgPath = strings.TrimSuffix(pkgPath, "_test")
	for _, p := range allowedPkgPrefixes {
		if pkgPath == p || strings.HasPrefix(pkgPath, p+"/") {
			return true
		}
	}
	return false
}

// hasOverride reports whether the import is silenced by a // forbidimport:ok
// <reason> comment on the same line as the import or on the line directly
// above. A bare marker with no trailing reason is rejected so override use
// must include a one-line justification reviewers can read.
func hasOverride(fset *token.FileSet, file *ast.File, imp *ast.ImportSpec) bool {
	importPos := fset.Position(imp.Pos())
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
			if !strings.HasPrefix(text, overrideMarker) {
				continue
			}
			rest := strings.TrimSpace(strings.TrimPrefix(text, overrideMarker))
			if rest == "" {
				continue
			}
			cPos := fset.Position(c.Pos())
			if cPos.Filename != importPos.Filename {
				continue
			}
			if cPos.Line == importPos.Line || cPos.Line+1 == importPos.Line {
				return true
			}
		}
	}
	return false
}
