# E2E tests for the upload form

Browser-driven tests that validate the SIN-62258 upload UI end-to-end:
client-side magic-byte gating, PT-BR error rendering, server-status
mapping, and WCAG 2.1 AA keyboard interaction with the cancel button.
Lives at `internal/e2e/upload/`. Owned by SIN-62270.

## Runner choice

We use **[chromedp](https://github.com/chromedp/chromedp)** — a pure-Go
library that drives Chrome/Chromium over the DevTools Protocol. The
candidates considered in SIN-62270 were:

| Runner | Pros | Cons |
| --- | --- | --- |
| Playwright | Largest ecosystem | Adds npm/pnpm dep + 200MB browser-binary download, runs JS test framework (foreign tooling) |
| go-rod | Pure-Go, ergonomic | Smaller community than chromedp, similar Chrome dependency |
| **chromedp** | Pure-Go, runs under `go test`, single Chrome process, no JS toolchain | Still needs a Chrome binary on `$PATH` |

chromedp wins on the boring-tech budget: it stays inside the Go
toolchain (CI already runs `go test`) and adds no new package manager.
The only out-of-Go dependency is the Chrome binary itself, which both
the developer laptop and the GitHub Actions `ubuntu-latest` runner
already provide.

## Running the suite

```bash
make e2e
```

Equivalent to `go test -tags=e2e -count=1 -timeout=120s ./internal/e2e/...`.
The `e2e` build tag keeps the browser-driven tests OUT of the regular
`go test ./...` pipeline; CI runs them in a separate job (see
[`.github/workflows/e2e.yml`](../.github/workflows/e2e.yml)).

Prerequisites:

- Go 1.23+ (the `go.mod` toolchain pin handles this).
- `google-chrome` (or `chromium`) on `$PATH`. `make e2e` checks for
  this and exits early with a friendly message if neither is found.

The whole suite runs in ~4 seconds on a warm cache (one Chrome process
per `go test` invocation, fresh tab per scenario).

## Debugging

### Verbose chromedp output

Set `SIN_E2E_DEBUG=1` to surface chromedp's RPC chatter and unhandled
event warnings:

```bash
SIN_E2E_DEBUG=1 make e2e
```

By default the test harness silences chromedp's `WithLogf` /
`WithErrorf` / `WithDebugf` channels because the upstream cdproto
package logs benign warnings for events Chrome adds before chromedp
catches up (e.g. the `Loopback` `IPAddressSpace` value).

### Headful mode

Drop the `--headless` flag and watch the browser:

```bash
SIN_E2E_HEADFUL=1 make e2e
```

Useful when a scenario times out and you want to see what the page
looks like at the moment of failure.

### Per-step diagnostics

Each scenario uses `runSteps` (see `internal/e2e/upload/helpers_test.go`)
which executes the chromedp actions one at a time and reports the exact
step name (e.g. `wait-classify-settled`) on failure. When a scenario
fails locally, look for the failing step name in the test output —
that's the line to start grepping from.

### Inspecting a hung page

Add `dumpPage(t, ctx)` from `helpers_test.go` to a failing scenario to
log the current URL, the spy buffer, and the entire `<html>` snapshot.
It's a no-op on success.

## Fixtures

Lives at `internal/adapter/web/upload/static/testdata/`. Note the path
is under `static/testdata` — `testdata` is a special directory name that
Go (and `//go:embed`) ignores when matching patterns, so these bytes are
never bundled into the production binary.

| File | Bytes | Purpose |
| --- | --- | --- |
| `logo.png` | 1×1 opaque red PNG (NRGBA, no compression) | Real PNG used by scenarios 2 and 4 |
| `logo.svg` | XML SVG text | Real SVG that scenario 1 expects the magic-byte gate to reject |
| `png-as-svg.svg` | PNG bytes saved with `.svg` extension | Scenario 2 — magic bytes win over filename |
| `exe-as-png.png` | DOS MZ stub (`4D 5A …`) saved with `.png` extension | Scenario 3 — magic bytes win over filename |

The vendored htmx is at `internal/e2e/upload/testdata/htmx.min.js`
(htmx 1.9.12, MIT licensed). It is **not** under the production
`static/` tree, so a typo in `embed` directives can't expose it via the
public `StaticHandler()`.

### Regenerating fixtures

Run the deterministic generator:

```bash
make e2e-fixtures
```

This invokes `go run
./internal/adapter/web/upload/static/testdata/gen_fixtures.go`. The
generator writes deterministic byte streams (no randomness, no
timestamps), so re-running on a different machine yields byte-identical
output. CI verifies this in the `e2e` workflow: if the committed
fixtures drift from the generator, the build fails with a diff.

### Adding a new fixture

1. Add the byte source to `gen_fixtures.go`.
2. `make e2e-fixtures` to write the file.
3. `git add internal/adapter/web/upload/static/testdata/<name>`.
4. Reference the new fixture from a test via `fixturePath(t,
   "<name>")`.

### Updating htmx

When a new htmx release ships, replace
`internal/e2e/upload/testdata/htmx.min.js` with the new bytes from
`https://unpkg.com/htmx.org@<version>/dist/htmx.min.js`. Pin the version
in this doc and in the commit message.

## Adding a new scenario

1. Decide what server response the scenario needs and pick (or write) a
   `postBehaviour` in `server_test.go` (`postOK`, `post415`, `postBoom`,
   …).
2. Add a `func TestE2E_<Name>(t *testing.T)` to `scenarios_test.go`.
3. Build a `[]namedAction` describing the steps. Keep them small and
   labelled; the labels show up in failure output.
4. Run `make e2e` locally three times in a row before raising the PR —
   if any of those runs flakes, fix the root cause (no `t.Skip`, no
   retries). See [SIN-62270](/SIN/issues/SIN-62270) acceptance
   criteria.

## Known constraints

- **Preview canvas as a "classify accepted" signal is unreliable.**
  When the file's MIME (derived from extension) disagrees with its
  magic-byte format, `URL.createObjectURL` produces a Blob with the
  wrong type and `Image()` fires `onerror` instead of `onload`. The
  canvas never unhides. `waitClassifySettled` polls for input-element
  state instead of relying on the canvas.
- **`htmx:xhr:progress` only fires for request-body upload, not
  download.** A 77-byte fixture finishes uploading in microseconds, so
  the cancel button never naturally appears via real progress. Scenario
  5 instead unhides the progress wrapper via the public
  `SinUpload.setProgress` helper to exercise the WCAG keyboard
  contract.
