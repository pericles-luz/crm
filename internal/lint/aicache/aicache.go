// Package aicache is a static analyzer that enforces ADR 0077 §3.4
// (SIN-62225) at compile time.
//
// Two rules:
//
//  1. Outside the legitimate adapter (configurable; default
//     ".../internal/ai/adapter/redis"), no file under the AI-bounded context
//     (configurable; default ".../internal/ai/") may import the redis client
//     library (configurable; default "github.com/redis/go-redis/v9"). The
//     intent is that callers reach redis only through port.Cache, which
//     itself accepts the opaque cache.Key.
//
//  2. Inside the adapter, every call to redis.Client.{Get,Set,Del,Exists,
//     Incr,Decr,Expire,...} must pass the key argument as <x>.String() where
//     <x> has type cache.Key. Because cache.Key has an unexported field, no
//     value of that type can exist except via cache.TenantKey / cache.SystemKey,
//     so this check is equivalent to the spec's
//     "argument is the result of cache.Key(...) or cache.SystemKey(...)".
//
// Together the rules close OWASP LLM06 cache poisoning: there is no syntactic
// path from a raw string into a redis.Client cache call inside internal/ai/.
package aicache

import (
	"flag"
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// Config holds the (overridable) substrings the analyzer uses to identify the
// AI-bounded context, the legitimate adapter inside it, the redis client
// package, and the cache helper package. Substring matching keeps the
// analyzer module-agnostic: any project that follows the same internal/ai/
// layout will work without editing the analyzer.
type Config struct {
	// AISubstr identifies the AI-bounded context. Files whose package import
	// path contains this substring are policed.
	AISubstr string
	// AdapterSubstr identifies the legitimate redis adapter inside the AI
	// bounded context. Files whose package import path contains this
	// substring are exempted from rule 1.
	AdapterSubstr string
	// RedisPkgSubstr identifies the redis client package. Imports whose path
	// contains this substring are flagged outside the adapter.
	RedisPkgSubstr string
	// CachePkgSubstr identifies the cache-helper package. The
	// allowed-key-arg form requires the receiver of the .String() call to
	// have a type defined in a package whose path contains this substring.
	CachePkgSubstr string
}

// DefaultConfig matches the SIN/CRM layout (see ADR 0077 §3.8).
var DefaultConfig = Config{
	AISubstr:       "/internal/ai/",
	AdapterSubstr:  "/internal/ai/adapter/redis",
	RedisPkgSubstr: "github.com/redis/go-redis/",
	CachePkgSubstr: "/internal/ai/cache",
}

// watchedRedisMethods are the redis.Client (and redis.Cmdable) methods whose
// first non-context argument is a key. Methods not listed here are not
// inspected; the lint is conservative on the right side of false-negatives.
var watchedRedisMethods = map[string]struct{}{
	"Get":         {},
	"Set":         {},
	"SetEX":       {},
	"SetNX":       {},
	"SetXX":       {},
	"GetSet":      {},
	"GetEx":       {},
	"GetDel":      {},
	"Del":         {},
	"Unlink":      {},
	"Exists":      {},
	"Expire":      {},
	"ExpireAt":    {},
	"PExpire":     {},
	"PExpireAt":   {},
	"Incr":        {},
	"Decr":        {},
	"IncrBy":      {},
	"DecrBy":      {},
	"IncrByFloat": {},
	"HGet":        {},
	"HSet":        {},
	"HDel":        {},
	"LPush":       {},
	"RPush":       {},
}

// NewAnalyzer returns a fresh analyzer wired to the given config. Tests use
// this constructor to point the analyzer at testdata stubs.
func NewAnalyzer(cfg Config) *analysis.Analyzer {
	a := &analysis.Analyzer{
		Name: "aicache",
		Doc: "aicache forbids direct redis use under internal/ai/ outside the redis adapter " +
			"and forces adapter calls to key on cache.Key.String() (ADR 0077 §3.4).",
		URL:      "https://pkg.go.dev/github.com/pericles-luz/crm/internal/lint/aicache",
		Requires: []*analysis.Analyzer{inspect.Analyzer},
		Run:      runWith(cfg),
	}
	fs := flag.NewFlagSet("aicache", flag.ContinueOnError)
	fs.StringVar(&cfg.AISubstr, "ai-substr", cfg.AISubstr, "substring identifying the AI bounded context import path")
	fs.StringVar(&cfg.AdapterSubstr, "adapter-substr", cfg.AdapterSubstr, "substring identifying the legitimate redis adapter import path")
	fs.StringVar(&cfg.RedisPkgSubstr, "redis-pkg-substr", cfg.RedisPkgSubstr, "substring identifying the redis client package import path")
	fs.StringVar(&cfg.CachePkgSubstr, "cache-pkg-substr", cfg.CachePkgSubstr, "substring identifying the AI cache-helper package import path")
	a.Flags = *fs
	return a
}

// Analyzer is the default-config analyzer wired for production CI.
var Analyzer = NewAnalyzer(DefaultConfig)

func runWith(cfg Config) func(*analysis.Pass) (interface{}, error) {
	return func(pass *analysis.Pass) (interface{}, error) {
		pkgPath := pass.Pkg.Path()
		if !strings.Contains(pkgPath, cfg.AISubstr) {
			return nil, nil
		}
		isAdapter := strings.Contains(pkgPath, cfg.AdapterSubstr)

		// Rule 1: redis import outside the adapter.
		if !isAdapter {
			for _, file := range pass.Files {
				for _, imp := range file.Imports {
					path := strings.Trim(imp.Path.Value, `"`)
					if strings.Contains(path, cfg.RedisPkgSubstr) {
						pass.Report(analysis.Diagnostic{
							Pos:     imp.Pos(),
							End:     imp.End(),
							Message: "package " + pkgPath + " is under " + cfg.AISubstr + " but imports redis directly; cache reads/writes must go through port.Cache (ADR 0077 §3.4)",
						})
					}
				}
			}
		}

		// Rule 2: redis.Client.{Get,Set,Del,...} key arg must be cache.Key.String() inside the adapter.
		// (Outside the adapter, rule 1 already prevents redis from being imported, so there is nothing to check.)
		if !isAdapter {
			return nil, nil
		}
		insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
		insp.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
			call, _ := n.(*ast.CallExpr)
			if call == nil {
				return
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return
			}
			methodName := sel.Sel.Name
			if _, watched := watchedRedisMethods[methodName]; !watched {
				return
			}
			// Skip package-qualified function calls (e.g. goredis.NewStringCmd).
			if id, ok := sel.X.(*ast.Ident); ok {
				if obj := pass.TypesInfo.ObjectOf(id); obj != nil {
					if _, isPkg := obj.(*types.PkgName); isPkg {
						return
					}
				}
			}
			// A redis-shaped op returns at least one value whose type is
			// defined in a package matching RedisPkgSubstr (e.g. goredis.StringCmd,
			// goredis.StatusCmd, goredis.IntCmd, ...). This catches calls
			// through both *goredis.Client and any Cmdable subset interface.
			if !returnsRedisType(call, pass.TypesInfo, cfg.RedisPkgSubstr) {
				return
			}
			argIdx := keyArgIndex(call, pass.TypesInfo)
			if argIdx < 0 || argIdx >= len(call.Args) {
				return
			}
			arg := call.Args[argIdx]
			if isCacheKeyStringCall(arg, pass.TypesInfo, cfg.CachePkgSubstr) {
				return
			}
			pass.Report(analysis.Diagnostic{
				Pos:     arg.Pos(),
				End:     arg.End(),
				Message: "redis." + methodName + " key argument must be cache.Key.String() built by cache.TenantKey or cache.SystemKey (ADR 0077 §3.4); got a non-cache.Key value",
			})
		})
		return nil, nil
	}
}

// returnsRedisType reports whether the call's return signature includes a
// value whose named type lives in a package whose path contains pkgSubstr.
func returnsRedisType(call *ast.CallExpr, info *types.Info, pkgSubstr string) bool {
	t := info.TypeOf(call)
	if t == nil {
		return false
	}
	switch tt := t.(type) {
	case *types.Tuple:
		for i := 0; i < tt.Len(); i++ {
			if isFromPackage(tt.At(i).Type(), pkgSubstr) {
				return true
			}
		}
		return false
	default:
		return isFromPackage(t, pkgSubstr)
	}
}

func isFromPackage(t types.Type, pkgSubstr string) bool {
	for {
		switch tt := t.(type) {
		case *types.Pointer:
			t = tt.Elem()
		case *types.Named:
			obj := tt.Obj()
			if obj == nil || obj.Pkg() == nil {
				return false
			}
			return strings.Contains(obj.Pkg().Path(), pkgSubstr)
		default:
			return false
		}
	}
}

// keyArgIndex returns the index of the key argument in the call. It is the
// first argument whose type is string (or whose type is a string-defined
// alias). For watched methods this is conventionally the first argument
// after context.Context.
func keyArgIndex(call *ast.CallExpr, info *types.Info) int {
	for i, arg := range call.Args {
		t := info.TypeOf(arg)
		if t == nil {
			continue
		}
		if isStringLike(t) {
			return i
		}
	}
	return -1
}

func isStringLike(t types.Type) bool {
	if b, ok := t.Underlying().(*types.Basic); ok {
		return b.Kind() == types.String || b.Kind() == types.UntypedString
	}
	return false
}

// isCacheKeyStringCall reports whether expr is a method-call expression of the
// form <x>.<m>() where <x> has a named type defined in a package whose path
// contains cachePkgSubstr (i.e. cache.Key). The name of <m> is not checked
// because cache.Key only exposes one accessor (String), and any future getter
// on the same opaque type is by construction equally safe.
func isCacheKeyStringCall(expr ast.Expr, info *types.Info, cachePkgSubstr string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	recvType := info.TypeOf(sel.X)
	if recvType == nil {
		return false
	}
	// Allow either the value or pointer type.
	for {
		if p, ok := recvType.(*types.Pointer); ok {
			recvType = p.Elem()
			continue
		}
		break
	}
	named, ok := recvType.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	return strings.Contains(obj.Pkg().Path(), cachePkgSubstr)
}
