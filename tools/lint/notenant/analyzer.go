// Package notenant implements a go/analysis pass that fails CI when any code
// under internal/ calls *pgxpool.Pool.{Exec,Query,QueryRow,SendBatch,CopyFrom}
// directly. Tenant-scoped DB access MUST go through the WithTenant /
// WithMasterOps helpers in internal/adapter/db/postgres, which is the only
// place where setting app.tenant_id and the master_ops audit row are
// guaranteed to happen. Direct pool access bypasses both. See SIN-62232 and
// docs/adr/0071-postgres-roles.md.
package notenant

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// Analyzer is the go/analysis pass that backs the `notenant` linter. Wire it
// up in CI with:
//
//	go vet -vettool=$(which notenant) ./internal/...
var Analyzer = &analysis.Analyzer{
	Name:     "notenant",
	Doc:      "reports direct *pgxpool.Pool.{Exec,Query,QueryRow,SendBatch,CopyFrom} calls in internal/; require WithTenant / WithMasterOps instead",
	Run:      run,
	Requires: []*analysis.Analyzer{inspect.Analyzer},
}

// forbiddenMethods is the set of pool methods that bypass tenant scoping.
// Begin / BeginTx / Acquire / BeginTxFunc remain allowed because WithTenant
// itself uses them.
var forbiddenMethods = map[string]struct{}{
	"Exec":      {},
	"Query":     {},
	"QueryRow":  {},
	"SendBatch": {},
	"CopyFrom":  {},
}

// pgxpoolPkgPath is the import path of the pgxpool subpackage; we match
// against the receiver type's package to decide whether to flag a call.
const pgxpoolPkgPath = "github.com/jackc/pgx/v5/pgxpool"

// allowedPkgPrefixes is the list of packages that are exempt from the rule.
// The adapter package is allowed because it IS the wrapper; the testpg
// harness is allowed because it uses pgxpool to bootstrap the test DB.
var allowedPkgPrefixes = []string{
	"github.com/pericles-luz/crm/internal/adapter/db/postgres",
}

func run(pass *analysis.Pass) (any, error) {
	if isAllowedPkg(pass.Pkg.Path()) {
		return nil, nil
	}
	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	insp.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return
		}
		method := sel.Sel.Name
		if _, forbidden := forbiddenMethods[method]; !forbidden {
			return
		}

		recvType := pass.TypesInfo.TypeOf(sel.X)
		if recvType == nil {
			return
		}

		// Strip pointer indirection: *pgxpool.Pool -> pgxpool.Pool.
		if ptr, ok := recvType.(*types.Pointer); ok {
			recvType = ptr.Elem()
		}
		named, ok := recvType.(*types.Named)
		if !ok {
			return
		}
		obj := named.Obj()
		if obj == nil || obj.Pkg() == nil {
			return
		}
		if obj.Pkg().Path() != pgxpoolPkgPath {
			return
		}
		typeName := obj.Name()
		if typeName != "Pool" && typeName != "Conn" {
			return
		}

		pass.Reportf(sel.Pos(),
			"direct *pgxpool.%s.%s bypasses tenant scoping; use postgres.WithTenant or postgres.WithMasterOps (SIN-62232 / ADR 0071)",
			typeName, method)
	})
	return nil, nil
}

func isAllowedPkg(path string) bool {
	for _, p := range allowedPkgPrefixes {
		// Also allow the external test package (path + "_test"), which is
		// what `package postgres_test` files compile to. The test code legitimately
		// connects directly to a pgxpool.Pool to assert RLS posture.
		if path == p || path == p+"_test" || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}
