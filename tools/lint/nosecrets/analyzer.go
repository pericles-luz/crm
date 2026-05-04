// Package nosecrets implements a go/analysis pass that fails CI when a
// log/print call site under the webhook + adapter packages references a
// forbidden secret or pre-HMAC tenant claim.
//
// Rules (ADR 0075 §5):
//
//  1. ALWAYS forbid "webhook_token", "raw_payload", and "Authorization" as
//     literal strings in arguments to a log/print/format call. Applies to
//     any package whose path contains the configured webhook OR adapter
//     substring.
//
//  2. PRE-HMAC forbid "tenant_id" and "tenant_slug" as literal strings in
//     log call arguments inside files that import the webhook package, when
//     the call appears textually before a `VerifyApp` (or `VerifyTenant`)
//     call in the same enclosing function. The conservative rule mirrors
//     ADR 0075 §2 D4 invariant F-9: pre-HMAC tenant identifiers are claims,
//     not authenticated facts, so they MUST NOT leak into log output.
//
// Override either rule for a single line with a `// nosecrets:ok <reason>`
// comment on (or immediately above) the offending statement.
//
// Wire it up in CI with the paperclip-lint multichecker (preferred):
//
//	paperclip-lint check ./...
//
// or as a stand-alone vet tool:
//
//	go vet -vettool=$(which paperclip-lint) ./...
package nosecrets

import (
	"flag"
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
)

// Config bundles the substrings that identify the package surface this
// analyzer polices. Substring matching keeps the analyzer module-agnostic.
type Config struct {
	// WebhookSubstr identifies the webhook package path. Files in such
	// packages are always policed and are treated as inherently webhook-
	// scoped for the pre-HMAC rule.
	WebhookSubstr string
	// AdapterSubstr identifies the adapter package paths. Files in such
	// packages are always policed for the always-forbidden secrets list.
	AdapterSubstr string
	// WebhookImportSubstr identifies the webhook package by import path.
	// A file that imports a path containing this substring becomes subject
	// to the pre-HMAC tenant-id rule.
	WebhookImportSubstr string
	// PreHMACGate is the unqualified function/method name that flips a
	// function out of "pre-HMAC" mode. Default "VerifyApp" matches the
	// ChannelAdapter contract in internal/webhook/ports.go.
	PreHMACGate string
}

// DefaultConfig matches the SIN/CRM layout per ADR 0075.
var DefaultConfig = Config{
	WebhookSubstr:       "/internal/webhook",
	AdapterSubstr:       "/internal/adapter",
	WebhookImportSubstr: "/internal/webhook",
	PreHMACGate:         "VerifyApp",
}

const suppressMarker = "nosecrets:ok"

// alwaysForbidden lists the literal substrings that may not appear as
// arguments (or formatted-string fragments) of a policed log call,
// regardless of pre/post HMAC scope.
var alwaysForbidden = []string{
	"webhook_token",
	"raw_payload",
	"Authorization",
}

// preHMACForbidden lists the literal substrings forbidden only in the pre-
// HMAC region of a webhook-importing file (F-9).
var preHMACForbidden = []string{
	"tenant_id",
	"tenant_slug",
}

// NewAnalyzer returns a fresh analyzer wired to cfg. Tests use this
// constructor to point the analyzer at testdata stubs.
func NewAnalyzer(cfg Config) *analysis.Analyzer {
	a := &analysis.Analyzer{
		Name:     "nosecrets",
		Doc:      "reports forbidden secret/identifier names in log/print call sites under webhook + adapter packages (ADR 0075 §5).",
		URL:      "https://pkg.go.dev/github.com/pericles-luz/crm/tools/lint/nosecrets",
		Run:      runWith(cfg),
		Requires: []*analysis.Analyzer{inspect.Analyzer},
	}
	fs := flag.NewFlagSet("nosecrets", flag.ContinueOnError)
	fs.StringVar(&cfg.WebhookSubstr, "webhook-substr", cfg.WebhookSubstr, "substring identifying the webhook package path (always policed)")
	fs.StringVar(&cfg.AdapterSubstr, "adapter-substr", cfg.AdapterSubstr, "substring identifying the adapter package paths (always policed)")
	fs.StringVar(&cfg.WebhookImportSubstr, "webhook-import-substr", cfg.WebhookImportSubstr, "substring identifying the webhook package import path (gates pre-HMAC tenant rule)")
	fs.StringVar(&cfg.PreHMACGate, "prehmac-gate", cfg.PreHMACGate, "unqualified call name that ends pre-HMAC scope (default VerifyApp)")
	a.Flags = *fs
	return a
}

// Analyzer is the default-config analyzer wired for production CI.
var Analyzer = NewAnalyzer(DefaultConfig)

func runWith(cfg Config) func(*analysis.Pass) (interface{}, error) {
	return func(pass *analysis.Pass) (interface{}, error) {
		pkgPath := pass.Pkg.Path()
		inWebhook := cfg.WebhookSubstr != "" && strings.Contains(pkgPath, cfg.WebhookSubstr)
		inAdapter := cfg.AdapterSubstr != "" && strings.Contains(pkgPath, cfg.AdapterSubstr)
		if !inWebhook && !inAdapter {
			return nil, nil
		}

		for _, file := range pass.Files {
			fileImportsWebhook := inWebhook
			if !fileImportsWebhook && cfg.WebhookImportSubstr != "" {
				for _, imp := range file.Imports {
					p, err := strconv.Unquote(imp.Path.Value)
					if err != nil {
						continue
					}
					if strings.Contains(p, cfg.WebhookImportSubstr) {
						fileImportsWebhook = true
						break
					}
				}
			}

			suppressed := suppressionLines(pass.Fset, file)

			ast.Inspect(file, func(n ast.Node) bool {
				switch fn := n.(type) {
				case *ast.FuncDecl:
					if fn.Body != nil {
						checkFunc(pass, cfg, fn.Body, fileImportsWebhook, suppressed)
					}
					return false
				case *ast.FuncLit:
					if fn.Body != nil {
						checkFunc(pass, cfg, fn.Body, fileImportsWebhook, suppressed)
					}
					return false
				case *ast.GenDecl:
					// Top-level var/const log calls are unusual but still in scope.
					checkNode(pass, cfg, fn, fileImportsWebhook, true, suppressed)
					return false
				}
				return true
			})
		}
		return nil, nil
	}
}

// checkFunc walks a function body, tracking whether the pre-HMAC gate has
// been crossed in source-textual order. Log calls before the gate are
// subject to the pre-HMAC rule; log calls after the gate get only the
// always-on rule. Nested function literals are visited separately.
func checkFunc(pass *analysis.Pass, cfg Config, body *ast.BlockStmt, fileImportsWebhook bool, suppressed map[int]bool) {
	preHMAC := fileImportsWebhook
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if _, ok := n.(*ast.FuncLit); ok {
			return false // visited separately
		}
		if call, ok := n.(*ast.CallExpr); ok {
			if preHMAC && isGateCall(call, cfg.PreHMACGate) {
				preHMAC = false
			}
			if isLogCall(pass, call) {
				checkCallArgs(pass, call, preHMAC, suppressed)
			}
		}
		return true
	})
}

// checkNode walks a non-function declaration looking for log calls. The
// pre-HMAC scope is always treated as the file's scope (we cannot detect a
// gate outside a function, so be conservative).
func checkNode(pass *analysis.Pass, cfg Config, n ast.Node, fileImportsWebhook bool, preHMAC bool, suppressed map[int]bool) {
	preHMAC = preHMAC && fileImportsWebhook
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isLogCall(pass, call) {
			checkCallArgs(pass, call, preHMAC, suppressed)
		}
		return true
	})
}

// checkCallArgs reports any forbidden literal substring inside `call`'s
// argument list. The full call AST is searched (not just direct args) to
// catch, e.g., slog.Group("authn", "tenant_id", id) where the forbidden
// label sits in a nested constructor.
func checkCallArgs(pass *analysis.Pass, call *ast.CallExpr, preHMAC bool, suppressed map[int]bool) {
	line := pass.Fset.Position(call.Pos()).Line
	if suppressed[line] {
		return
	}

	forbidden := alwaysForbidden
	if preHMAC {
		// Make a copy so we don't append to the package-level slice across
		// passes.
		merged := make([]string, 0, len(alwaysForbidden)+len(preHMACForbidden))
		merged = append(merged, alwaysForbidden...)
		merged = append(merged, preHMACForbidden...)
		forbidden = merged
	}

	// Track the first hit so we report a single, focused diagnostic per
	// call. Reporting on the offending literal (not the call) lets the
	// developer jump straight to the bad arg.
	var hitPos token.Pos
	var hitLabel string

	ast.Inspect(call, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		val, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		lower := strings.ToLower(val)
		for _, f := range forbidden {
			if strings.Contains(lower, strings.ToLower(f)) {
				if hitPos == token.NoPos {
					hitPos = lit.Pos()
					hitLabel = f
				}
				return false
			}
		}
		return true
	})

	if hitPos == token.NoPos {
		return
	}
	scope := "webhook scope"
	if preHMAC && isPreHMACOnly(hitLabel) {
		scope = "pre-HMAC scope (F-9)"
	}
	pass.Reportf(hitPos,
		"log/print call references forbidden field %q in %s (ADR 0075 §5); add `// %s <reason>` to the line to whitelist",
		hitLabel, scope, suppressMarker)
}

func isPreHMACOnly(label string) bool {
	for _, p := range preHMACForbidden {
		if p == label {
			return true
		}
	}
	return false
}

// isGateCall reports whether call invokes a method/function whose
// unqualified name equals gate (typically "VerifyApp"). Both
// `<x>.VerifyApp(...)` and a bare `VerifyApp(...)` count.
func isGateCall(call *ast.CallExpr, gate string) bool {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fn.Sel != nil && fn.Sel.Name == gate
	case *ast.Ident:
		return fn.Name == gate
	}
	return false
}

// isLogCall reports whether call is a log/print/format call we want to
// police: anything in package `log`, `log/slog`, or `fmt.Print*`/`Errorf`,
// and any method on a *log.Logger or *slog.Logger receiver. The list is
// chosen to match the routes by which strings end up in stdout/stderr or in
// a logging backend.
func isLogCall(pass *analysis.Pass, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil {
		return false
	}
	method := sel.Sel.Name

	// Package-qualified: log.X, slog.X, fmt.X.
	if id, isIdent := sel.X.(*ast.Ident); isIdent {
		if obj := pass.TypesInfo.ObjectOf(id); obj != nil {
			if pn, ok := obj.(*types.PkgName); ok {
				switch pn.Imported().Path() {
				case "log":
					return isLogPkgFunc(method)
				case "log/slog":
					return isSlogPkgFunc(method)
				case "fmt":
					return isFmtPrintFunc(method)
				}
			}
		}
	}

	// Method on *log.Logger or *slog.Logger.
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
	switch obj.Pkg().Path() {
	case "log":
		return obj.Name() == "Logger" && isLogPkgFunc(method)
	case "log/slog":
		return obj.Name() == "Logger" && isSlogPkgFunc(method)
	}
	return false
}

func isLogPkgFunc(name string) bool {
	switch name {
	case "Print", "Printf", "Println",
		"Fatal", "Fatalf", "Fatalln",
		"Panic", "Panicf", "Panicln",
		"Output":
		return true
	}
	return false
}

func isSlogPkgFunc(name string) bool {
	switch name {
	case "Debug", "DebugContext",
		"Info", "InfoContext",
		"Warn", "WarnContext",
		"Error", "ErrorContext",
		"Log", "LogAttrs":
		return true
	}
	return false
}

func isFmtPrintFunc(name string) bool {
	switch name {
	case "Print", "Printf", "Println",
		"Fprint", "Fprintf", "Fprintln",
		"Sprint", "Sprintf", "Sprintln",
		"Errorf":
		return true
	}
	return false
}

// suppressionLines returns the set of source-file line numbers carrying a
// `// nosecrets:ok ...` marker. A diagnostic on the SAME line, or on the
// line immediately following the marker, is silenced.
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
			line := fset.Position(c.Pos()).Line
			out[line] = true
			out[line+1] = true
		}
	}
	return out
}
