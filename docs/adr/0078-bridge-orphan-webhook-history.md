# ADR 0078 — Bridging the orphan webhook stack into `main` via `--allow-unrelated-histories`

> **Status:** Accepted (CTO + CEO authorization on [SIN-62292](/SIN/issues/SIN-62292) decisão #12 and parent [SIN-62296](/SIN/issues/SIN-62296)).
> **Owner:** [@Coder](agent://d50411f4-2d31-4ddf-82e4-a0b3acf4d398) for execution; PR-open + Docker smoke + merge are Pericles-only.
> **Source issue:** [SIN-62297](/SIN/issues/SIN-62297).
> **Related:** [ADR 0075](./0075-webhook-security.md) (the architecture that lives on the orphan stack).
> **Lenses applied:** Reversibility & blast radius, Boring technology budget, Defense in depth.

## 1. Contexto

The webhook security stack landed across five PRs ([SIN-62234](/SIN/issues/SIN-62234), [SIN-62275](/SIN/issues/SIN-62275), [SIN-62277](/SIN/issues/SIN-62277), [SIN-62279](/SIN/issues/SIN-62279), [SIN-62281](/SIN/issues/SIN-62281)) on a private fork (`pericles-luz/crm`). All five PRs were authored against an **orphan branch** (`feat/sin-62234-webhook-security`) whose root commit `d17249e` shares no merge base with `pericles-luz/crm@main`.

Concretely, on 2026-05-05:

- `origin/main` tip = `b5885d19` (PR #27).
- `origin/feat/sin-62234-webhook-security` tip = `3c7740a4` (PR #3 into the orphan, brings 62234 + 62275 + 62279).
- `origin/feat/sin-62277-integration-tests` tip = `932c33f` (sibling on the same orphan tree, brings 62277 + 62281 atop the same `94779a1` SecurityEngineer-reviewed base).
- `gh api repos/pericles-luz/crm/compare/main...feat/sin-62234-webhook-security` returns 404 "No common ancestor".

`main` cannot ingest the orphan via cherry-pick or rebase without rewriting commits that already received SecurityEngineer + CTO sign-off, and a `git reset` of `main` to the orphan would discard PRs already merged via `main` (PRs #5 / #27, etc.).

## 2. Decisão

Cut a bridge branch `feat/sin-62296-bridge-webhook-to-main` from the orphan tip and create **two** merge commits on it:

1. **Consolidate the orphan stack.** Merge `origin/feat/sin-62277-integration-tests` (no-ff). This unifies the two sibling tips that share base `94779a1`, so all five SIN-62234..62281 issues ride on one tip before crossing the unrelated-histories boundary. This was a deviation from the original [SIN-62297](/SIN/issues/SIN-62297) plan, which assumed all five issues already lived on `feat/sin-62234-webhook-security` — see §6 below.
2. **Bridge to main.** `git merge origin/main --allow-unrelated-histories --no-ff`. This is the single Pareto-optimal point in history where the two trees meet; everything downstream is a normal merge.

The PR back into the upstream `pericles-luz/crm@main` is opened by Pericles (per decisão #12 on [SIN-62292](/SIN/issues/SIN-62292)). Coder only prepares and pushes the conflict-resolved branch.

## 3. Por que `--allow-unrelated-histories` (e não outras opções)

| Alternativa | Por que não |
|---|---|
| `git rebase` orphan onto `main` | Rewrites the SecurityEngineer-reviewed commits (`b055da5`, `94779a1`). Loses the `Reviewed-by` chain and forces re-review of every patch. Fails the `Reversibility` and `Defense in depth` lenses (review history is a control). |
| Cherry-pick each commit | Same review-history loss as rebase, multiplied by N commits. Also loses the merge commits that record which sibling branch contributed which work, which matters for the orphan-internal §1 consolidation. |
| `git reset --hard` `main` to the orphan tip | Destructive: deletes work already merged into `main` (PR #5 paperclip-lint, PR #27 and the SIN-62215 deploy runbooks). Fails `Reversibility & blast radius`. |
| Deferred: re-author the webhook stack on a fresh branch off `main` | Highest cost, no functional gain. SecurityEngineer review and CEO authorization are bound to the *content*, not the SHAs, but losing the merge graph erases the audit trail. |
| `--allow-unrelated-histories` (chosen) | Native git, single merge commit per direction, fully reversible by `git revert <merge>` or branch-deletion. Preserves every original SHA and review record. Matches the **Boring technology budget** lens — no custom rebase scripts. |

## 4. Conflitos resolvidos e regras aplicadas

The unrelated-histories merge surfaced six `add/add` conflicts. The post-consolidation orphan tip has all five issues' content; the table shows the resolution for each conflicting path:

| File | Side taken | Reasoning |
|---|---|---|
| `Makefile` | **union** | Orphan side adds `test-integration` + `test-integration-cover` (SIN-62277 quality bar) and `ITEST_*` vars. Main side has `lint`/`notenant`/`lint-aicache`/`migrate-up`/`migrate-down`/`seed-stg`/`verify-vendor`. Final file is a hand-merged union: main's evolved targets + orphan's webhook integration targets, single `.PHONY` line covering both. |
| `deploy/caddy/Caddyfile` | **main** | Orphan side is the Phase-0 bootstrap (commit `d17249e`); main has security-headers extracted into `security-headers.caddy` (ADR 0082 §1, [SIN-62229](/SIN/issues/SIN-62229) / [SIN-62283](/SIN/issues/SIN-62283)). The orphan version predates the security-headers refactor and offers nothing the main version doesn't already have. |
| `deploy/compose/.env.example` | **main** | Same reason as Caddyfile — orphan is the bootstrap snapshot, main has staging/production additions. |
| `deploy/compose/compose.yml` | **main** | Same reason. |
| `go.mod` | **union, then `go mod tidy`** | Took the higher of duplicate requires (`pgx/v5 v5.9.2 > v5.5.5`, `go 1.25.0 > 1.24`), unioned all direct requires (orphan: `prometheus/client_golang`, `prometheus/client_model`, `testcontainers-go`, `testcontainers-go/modules/postgres`; main: `google/uuid`, `redis/go-redis/v9`, `golang.org/x/tools`), and let `go mod tidy` resolve indirects. |
| `go.sum` | **regenerated** | Discarded both sides and let `go mod tidy` recompute checksums against the union `go.mod`. |

### 4.1. `replace golang.org/x/tools` directive

`go mod tidy` selected `golang.org/x/tools v0.43.0` via MVS (transitively required by `golang.org/x/text v0.36.0`, which is in turn required by the OTel chain pulled in by `testcontainers-go`). At v0.41+ the `analysistest` package deadlocks when loading the `tools/lint/nobodyreread/testdata/src/middlewarepkg` fixture (`(*loader).loadPackage` blocks on `chan send`). The same testdata passes on `main@b5885d19` with `x/tools v0.21.1`.

Resolution: add `replace golang.org/x/tools => golang.org/x/tools v0.40.0` so the analyzer tests keep working without disturbing the testcontainers chain. v0.40.0 is the highest version that loads `analysistest` testdata cleanly; v0.21.1 (main's pin) was tested first but fails to compile under go 1.25 (`internal/tokeninternal/tokeninternal.go:64` rejects a constant array length). Only `internal/lint/aicache` and `tools/lint/*` import `x/tools` at this repo's boundary; testcontainers does not call x/tools at runtime, so the pin is safe. This is documented as a follow-up: a future SIN ticket should re-evaluate the analyzer fixtures against `analysistest` ≥ v0.41 and remove the replace.

## 5. Rollback path

The bridge is exactly **two** merge commits on top of the orphan tip. Either of these one-step recoveries fully reverses it:

- **Branch-level rollback (preferred while no PR is open):** `git push origin --delete feat/sin-62296-bridge-webhook-to-main` and re-cut from a clean orphan tip. No `main` history is ever touched on `pericles-luz/crm` until Pericles opens the PR.
- **Post-merge rollback (after Pericles merges into `main`):** `git revert -m 1 <bridge-merge-sha>` on `main` reverses the unrelated-histories merge while keeping the webhook code reachable from the bridge branch for future re-attempts. The orphan-internal merge (commit `674962c`) does not need to be reverted separately — the revert of the outer merge removes the entire orphan subgraph from `main`'s effective tree.

There is no schema or runtime state to roll back: this ADR records a pure history reconciliation. Migrations, feature flags, and runtime configuration are unchanged.

## 6. Deviations from the SIN-62297 plan

- **Plan step 1** said "Branch off `origin/feat/sin-62234-webhook-security`" with the implicit assumption that the orphan tip already contained all five SIN-62234..62281 issues. It did not — `feat/sin-62234-webhook-security` (`3c7740a`) carried 62234 + 62275 + 62279 only, while 62277 + 62281 lived on the sibling `feat/sin-62277-integration-tests` (`932c33f`). To honor the acceptance criterion that `TestTG5_ReplayWindowViolation` MUST be present, an extra merge commit (§2 step 1) was inserted **before** the unrelated-histories merge to consolidate the two sibling tips. Both branches share base `94779a1`, so the extra merge is purely additive.
- **Acceptance check `find internal/webhook/integration -name 'tg5_replay*'`** returns nothing because the T-G5 test is hosted in `internal/webhook/integration/webhook_gaps_test.go` (per the SIN-62281 commit message), not a standalone `tg5_replay*.go` file. The functional intent — the regression gate is present and runnable — is satisfied: `grep -rn TestTG5_ReplayWindowViolation internal/webhook/integration/` returns the test at `webhook_gaps_test.go:86`.
- **`replace golang.org/x/tools`** (§4.1) was added to keep `go test ./tools/lint/nobodyreread/...` green after `go mod tidy` raised the version past the regression. The plan did not anticipate this; flagging here so the CTO can decide whether to keep the replace or push for a x/tools upgrade ticket.

## 7. Lenses

- **Reversibility & blast radius.** Two merge commits, each with a clear single-direction revert path. The branch is delete-and-recut-able until Pericles opens the upstream PR. After PR merge, a single `git revert -m 1` on `main` reverses both the unrelated-histories bridge and the orphan subgraph in one operation.
- **Boring technology budget.** Vanilla git: `--allow-unrelated-histories` plus `--no-ff`. No custom scripts, no third-party rewriters, no submodule trickery. The conflict resolutions are bounded to six files.
- **Defense in depth.** Local `go vet`, `staticcheck`, and `go test ./...` are gates 1–3 and run before push. Pericles' Docker smoke and CI gates are gates 4–5 on PR open. Each layer has a different failure mode (compile-time, vet-time, unit-test-time, container-runtime, CI-environment), so no single missed check means a silent regression.

## 8. Postmortem follow-up — `docker-smoke` PR-time gate ([SIN-62301](/SIN/issues/SIN-62301))

The bridge merge of this ADR (PR #28) was 8/8 green at PR-time and red 23s after merge: `cd-stg` ([run 25413412619](https://github.com/pericles-luz/crm/actions/runs/25413412619)) failed at `[builder 4/7] RUN go mod download` because the `Dockerfile` builder was pinned at `golang:1.24.5-alpine` while `go.mod` had been bumped to `toolchain go1.25.9` in commit `c4b2c73`. None of the eight PR-time CI jobs exercised `docker build` — they all use `actions/setup-go`, which installs Go independently of the Dockerfile builder image — so the Dockerfile-vs-`go.mod` consistency was structurally uncheckable until after merge.

[SIN-62301](/SIN/issues/SIN-62301) closes that gap by adding `.github/workflows/docker-smoke.yml`, a build-only smoke that runs the multi-stage `builder` target on every PR that touches `Dockerfile`, `.dockerignore`, `go.mod`, or `go.sum`. It is fail-closed and complementary to (not a duplicate of) [SIN-62298](/SIN/issues/SIN-62298) (`govulncheck` source-mode CVE gate) — see `docs/deploy/staging.md` → "Pre-merge gates" for the table.

Process rule (call-out for future bridge / toolchain / image-bump work): **whenever you change `go.mod`'s `go` or `toolchain` directive, bump the `Dockerfile` builder image and the dev compose Go base in the same PR.** The `docker-smoke` gate fails closed on the PR if the Dockerfile lags, but the gate is a backstop, not a replacement for the operator-side checklist. The pinned digests to update are listed in `docs/deploy/staging.md` → "Pre-merge gates".
