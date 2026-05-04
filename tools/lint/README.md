# paperclip-lint

A bundle of project-specific `go/analysis` passes that gate webhook security
invariants from ADR 0075. Each pass is small, AST-based (no regex), and has
its own analystest fixture suite. The bundle is shipped as a single binary
so CI only needs one tool.

## Analyzers

| Name            | What it flags                                                                                                            | ADR |
|-----------------|--------------------------------------------------------------------------------------------------------------------------|-----|
| `nobodyreread`  | `*http.Request.Body` read more than once in the same handler/middleware, or any read inside a `func(http.Handler) http.Handler` middleware. `http.MaxBytesReader` is OK (wraps, does not consume). | 0075 §2 D2 / F-7 rev 3 |
| `nomathrand`    | `math/rand` (or `math/rand/v2`) imported under `internal/webhook/` or in any file ending in `gen.go`. Webhook token material MUST come from `crypto/rand`. | 0075 §2 D1 / F-10 |
| `nosecrets`     | Forbidden labels in `log` / `slog` / `fmt.Print*` / `fmt.Errorf` calls under `internal/webhook/` and `internal/adapter/`. Always-forbidden: `webhook_token`, `raw_payload`, `Authorization`. Pre-HMAC forbidden (file imports `internal/webhook` and call sits before `VerifyApp`): `tenant_id`, `tenant_slug`. | 0075 §5 / F-9 |

All three analyzers can be silenced for one line by adding a comment marker:

```go
log.Printf("claim tenant_id=%s for forensics", id) // nosecrets:ok forensic capture, post-HMAC redacted downstream
_, _ = io.ReadAll(r.Body)                          // nobodyreread:ok intentional drain after replay store write
import _ "math/rand"                                // nomathrand:ok perf-test fixture, never linked into prod
```

The marker MUST sit on the same line as the diagnostic OR on the line
immediately above. Real production overrides should include a one-line
justification — these markers are reviewed in PR.

## Running locally

The recommended local invocation matches CI: build the binary once, then
drive it via `go vet`.

```sh
go build -o bin/paperclip-lint ./cmd/paperclip-lint
go vet -vettool=$(pwd)/bin/paperclip-lint ./internal/webhook/... ./internal/adapter/...
```

The bundled `check` subcommand is a convenience wrapper around the
multichecker binary and accepts the same package patterns:

```sh
./bin/paperclip-lint check ./internal/webhook/... ./internal/adapter/...
```

Run a single analyzer with its dot-prefixed flags (example: override the
default pre-HMAC gate name):

```sh
go vet -vettool=$(pwd)/bin/paperclip-lint -nosecrets.prehmac-gate=VerifyTenant ./internal/webhook/...
```

## Tests

Each analyzer has a `analyzer_test.go` running `analysistest` against the
testdata fixtures. The fixtures cover regression patterns (must flag), the
happy path (must stay silent), and corner cases (override markers, nested
function literals, methods on `*log.Logger`, adapter packages that do not
import webhook).

```sh
go test ./tools/lint/...
```

## Adding a new analyzer

1. Add a new package under `tools/lint/<name>/` with `analyzer.go` +
   `analyzer_test.go` + `testdata/src/...`.
2. Register it in `cmd/paperclip-lint/main.go` next to the existing
   analyzers.
3. Add the relevant test paths to `.github/workflows/paperclip-lint.yml`.
4. Document the rule in this README and reference the ADR that motivates
   it.
