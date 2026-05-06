# GitHub Actions workflows

PR 8/12 of Fase 0 (SIN-62210). Triggered on `push` to `main` and on every
`pull_request` targeting `main`. No secrets are required — secrets land in
PR9 (CD stg) only.

## `ci.yml`

| Job             | Purpose                                                                                              |
| --------------- | ---------------------------------------------------------------------------------------------------- |
| `lint`          | `gofmt -l .`, `go vet ./...`, `staticcheck ./...`, plus the SIN-62232 notenant analyzer + self-test. |
| `test`          | `go test -race -coverprofile=cov.out ./...` against postgres / redis / nats `services:` containers.  |
| `coverage-gate` | Downloads `cov.out` and runs `scripts/coverage-check.sh cov.out 85` — fails below 85 % total stmts.  |
| `build`         | `go build ./cmd/server` to catch link-time regressions independently of the test job.                |

Caching: `actions/setup-go@v5` with `cache: true` reuses the module + build
caches keyed off `go.sum`. Service images mirror `deploy/compose/compose.yml`
(`postgres:16-alpine`, `redis:7-alpine`, `nats:2-alpine`) so local and CI
behaviour stay aligned. See `scripts/coverage-check.sh` for the gate script.

## Other workflows in this directory

- `aicache-lint.yml` — runs the SIN-62236 aicache singlechecker against
  `./internal/ai/...` (kept separate so it can iterate independently of `ci`).
- `paperclip-lint.yml` — runs the paperclip-lint analyzers (nobodyreread,
  nosecrets, nomathrand) against the tree.
- `govulncheck.yml` — SIN-62298 security gate. Uses
  `golang/govulncheck-action@v1` in source mode to fail PRs that
  introduce a *called* stdlib or dependency CVE. Imported-but-uncalled
  and module-required-but-uncalled findings are reported but
  non-failing. Full `-show verbose` output is uploaded as the
  `govulncheck-verbose` artifact (30d retention) so ADR residual-risk
  sections and incident triage can cite a stable build URL.
