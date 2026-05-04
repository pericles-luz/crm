// Package nobodyreread implements a go/analysis pass that fails CI when an
// HTTP handler or middleware reads *http.Request.Body more than once, or when
// any function shaped like middleware (`func(http.Handler) http.Handler`)
// reads r.Body at all. Both patterns break ADR 0075 §2 D2 / F-7 (rev 3) body
// bit-exactness — the same []byte must feed HMAC verify and the idempotency
// key. Re-reading r.Body returns an empty buffer to the second consumer, which
// silently breaks signature verification and dedup. Body-size limits applied
// via http.MaxBytesReader are explicitly OK because they wrap, not consume.
//
// Wire it up in CI with the paperclip-lint multichecker (preferred):
//
//	paperclip-lint check ./...
//
// or as a stand-alone vet tool:
//
//	go vet -vettool=$(which paperclip-lint) ./...
//
// Override an intentional second read with a `// nobodyreread:ok <reason>`
// line comment on (or immediately above) the offending statement.
package nobodyreread

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// Analyzer is the go/analysis pass that backs the `nobodyreread` linter.
var Analyzer = &analysis.Analyzer{
	Name:     "nobodyreread",
	Doc:      "reports re-reads of *http.Request.Body inside a single handler/middleware (ADR 0075 §2 D2 / F-7 rev 3).",
	URL:      "https://pkg.go.dev/github.com/pericles-luz/crm/tools/lint/nobodyreread",
	Run:      run,
	Requires: []*analysis.Analyzer{inspect.Analyzer},
}

// httpReqPkg is the import path of the package defining the Request type
// whose Body field we police. Substring match keeps the analyzer working for
// the std net/http and a fixture stub.
const httpReqPkg = "net/http"

// suppressMarker is the comment marker that disables the lint for one line.
const suppressMarker = "nobodyreread:ok"

func run(pass *analysis.Pass) (interface{}, error) {
	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	suppressed := suppressionLines(pass)

	insp.Preorder([]ast.Node{(*ast.FuncDecl)(nil), (*ast.FuncLit)(nil)}, func(n ast.Node) {
		var body *ast.BlockStmt
		var ftype *ast.FuncType
		var fnName string
		switch f := n.(type) {
		case *ast.FuncDecl:
			body = f.Body
			ftype = f.Type
			if f.Name != nil {
				fnName = f.Name.Name
			} else {
				fnName = "<anon>"
			}
		case *ast.FuncLit:
			body = f.Body
			ftype = f.Type
			fnName = "<func literal>"
		}
		if body == nil {
			return
		}

		// Rule 1: re-read in same function/handler. Scan only direct reads —
		// nested function literals form their own scopes and are visited
		// separately by Preorder.
		direct := dropSuppressed(pass, collectBodyReads(pass, body, false), suppressed)
		if len(direct) >= 2 {
			for _, r := range direct {
				pass.Reportf(r.Pos(),
					"r.Body is read more than once in %s; webhook handlers must read body exactly once "+
						"(ADR 0075 §2 D2 / F-7) — read once with io.ReadAll and reuse the []byte, or "+
						"add a // %s justification",
					fnName, suppressMarker)
			}
		}

		// Rule 2: middleware-shaped functions (`func(http.Handler) http.Handler`)
		// must not consume r.Body at all — that would break /webhooks/*
		// bit-exactness. (Rev 3 refinement.) Middleware typically returns
		// http.HandlerFunc(func(w, r) { ... }), so we MUST descend through
		// nested function literals to catch reads inside the returned handler.
		if isMiddlewareSignature(pass, ftype) {
			deep := dropSuppressed(pass, collectBodyReads(pass, body, true), suppressed)
			for _, r := range deep {
				pass.Reportf(r.Pos(),
					"middleware %s reads r.Body; routes matching /webhooks/* require body bit-exactness "+
						"(ADR 0075 §2 D2 / F-7 rev 3) — middleware MUST NOT consume body. "+
						"http.MaxBytesReader is OK; Read/ReadAll/Decode are not. Override with // %s if intentional",
					fnName, suppressMarker)
			}
		}
	})

	return nil, nil
}

// collectBodyReads walks `body` and returns every consuming read of an
// *http.Request.Body. When descend is false, nested function literals form
// their own scopes and are skipped (they are visited separately by Preorder).
// When descend is true, all nested function literals are also scanned —
// used by the middleware rule, where the read inside the returned handler
// is still the middleware's responsibility.
func collectBodyReads(pass *analysis.Pass, body *ast.BlockStmt, descend bool) []*ast.CallExpr {
	var out []*ast.CallExpr
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if !descend {
			if _, isFuncLit := n.(*ast.FuncLit); isFuncLit {
				return false // visited separately
			}
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isBodyConsumingCall(pass, call) {
			out = append(out, call)
		}
		return true
	})
	return out
}

// dropSuppressed filters out reads whose source line carries (or sits one
// line below) a `// nobodyreread:ok` marker.
func dropSuppressed(pass *analysis.Pass, reads []*ast.CallExpr, suppressed map[int]bool) []*ast.CallExpr {
	out := reads[:0]
	for _, r := range reads {
		line := pass.Fset.Position(r.Pos()).Line
		if suppressed[line] {
			continue
		}
		out = append(out, r)
	}
	return out
}

// isBodyConsumingCall reports whether `call` consumes a request body. The
// canonical accept-list of NON-consuming forms is hard-wired:
//
//   - http.MaxBytesReader(w, r.Body, n)        — wraps for size limit, no IO
//   - r.Body.Close()                            — releases, does not read
//
// All other call expressions whose argument list (or receiver chain) reaches
// `<x>.Body` where <x> has type *net/http.Request are treated as a consuming
// read. This includes the common offenders:
//
//   - io.ReadAll(r.Body) / ioutil.ReadAll(r.Body)
//   - r.Body.Read(buf)
//   - json.NewDecoder(r.Body).Decode(&v)
//   - bufio.NewReader(r.Body).ReadString(...)
func isBodyConsumingCall(pass *analysis.Pass, call *ast.CallExpr) bool {
	if isMaxBytesReader(call) {
		return false
	}

	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		// `r.Body.Close()` — receiver is r.Body, method is Close: not a read.
		if sel.Sel != nil && sel.Sel.Name == "Close" && isReqBody(pass, sel.X) {
			return false
		}
		// `r.Body.Read(...)` — flag.
		if sel.Sel != nil && sel.Sel.Name == "Read" && isReqBody(pass, sel.X) {
			return true
		}
	}

	// Anywhere the call's argument list mentions r.Body, treat as consuming.
	for _, a := range call.Args {
		if isReqBody(pass, a) {
			return true
		}
	}

	// Method chain: `<inner>.<method>(...)` where <inner> is a CallExpr that
	// itself receives r.Body (e.g. json.NewDecoder(r.Body).Decode(&v) — the
	// outer call is `<inner>.Decode(&v)`, the consuming step is the
	// constructor inside the chain that wires the body in. We catch that
	// because `json.NewDecoder` itself is visited as its own CallExpr by the
	// caller of this function (Preorder visits all CallExprs).
	return false
}

// isMaxBytesReader reports whether call is `<pkg>.MaxBytesReader(...)` of
// any package. Matching by selector name keeps fixtures simple — net/http
// exposes the only widely-used MaxBytesReader.
func isMaxBytesReader(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil {
		return false
	}
	return sel.Sel.Name == "MaxBytesReader"
}

// isReqBody reports whether expr is `<x>.Body` where <x> resolves to type
// *net/http.Request (or the unnamed pointer to it). The test type-checks
// against the package whose path equals httpReqPkg.
func isReqBody(pass *analysis.Pass, expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil || sel.Sel.Name != "Body" {
		return false
	}
	t := pass.TypesInfo.TypeOf(sel.X)
	if t == nil {
		return false
	}
	for {
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
			continue
		}
		break
	}
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	return obj.Pkg().Path() == httpReqPkg && obj.Name() == "Request"
}

// isMiddlewareSignature reports whether ft is shaped like
// `func(http.Handler) http.Handler` — the canonical middleware adapter form
// used by chi, mux.Use, and stdlib. Both the single parameter and single
// result must resolve to http.Handler.
func isMiddlewareSignature(pass *analysis.Pass, ft *ast.FuncType) bool {
	if ft == nil || ft.Params == nil || ft.Results == nil {
		return false
	}
	if len(ft.Results.List) != 1 {
		return false
	}
	// Count params (a Field can carry multiple names).
	paramCount := 0
	for _, f := range ft.Params.List {
		if len(f.Names) == 0 {
			paramCount++
		} else {
			paramCount += len(f.Names)
		}
	}
	if paramCount != 1 {
		return false
	}
	if !isHTTPHandlerType(pass, ft.Params.List[0].Type) {
		return false
	}
	if !isHTTPHandlerType(pass, ft.Results.List[0].Type) {
		return false
	}
	return true
}

// isHTTPHandlerType reports whether expr's type is net/http.Handler.
func isHTTPHandlerType(pass *analysis.Pass, expr ast.Expr) bool {
	t := pass.TypesInfo.TypeOf(expr)
	if t == nil {
		return false
	}
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	return obj.Pkg().Path() == httpReqPkg && obj.Name() == "Handler"
}

// suppressionLines returns the set of source-file line numbers carrying a
// `// nobodyreread:ok ...` marker. A diagnostic on the SAME line, or on the
// line immediately following the marker, is silenced.
func suppressionLines(pass *analysis.Pass) map[int]bool {
	out := make(map[int]bool)
	for _, f := range pass.Files {
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
				text = strings.TrimSpace(strings.TrimPrefix(text, "/*"))
				text = strings.TrimSpace(strings.TrimSuffix(text, "*/"))
				if !strings.HasPrefix(text, suppressMarker) {
					continue
				}
				line := pass.Fset.Position(c.Pos()).Line
				out[line] = true
				out[line+1] = true
			}
		}
	}
	return out
}

