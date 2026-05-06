# Import rules — hexagonal boundary enforcement

This document describes the static rules that keep the CRM's hexagonal
boundary intact. The rules are enforced in CI by the `forbidimport`
analyzer (SIN-62216). Without them, domain code drifts toward direct DB
calls and the boundary erodes.

## The rule

> **Only the postgres adapter may import a SQL driver.**

The postgres adapter is split across two sibling sub-packages —
`internal/adapter/db/postgres/...` (pool, tenant scoping, testpg
harness) and `internal/adapter/store/postgres/...` (per-port store
implementations). Together they form the seam.

Concretely, the following packages MUST NOT appear in any `import`
declaration outside those two adapter sub-trees:

| Forbidden import          | Match  | Why                                                |
| ------------------------- | ------ | -------------------------------------------------- |
| `database/sql`            | exact  | stdlib SQL driver shim                             |
| `database/sql/driver`     | exact  | stdlib driver-author API                           |
| `github.com/jackc/pgx`    | prefix | covers `pgx`, `pgx/v5`, `pgx/v5/pgxpool`, `pgconn` |
| `github.com/jackc/pgconn` | prefix | covers any pgconn major version                    |
| `github.com/lib/pq`       | exact  | legacy PostgreSQL driver                           |

Use case packages, HTTP handlers, domain models, and orchestration code
depend on **ports** (interfaces) — never on the driver.

## Allowed paths

The analyzer's allowlist is intentionally narrow — only the two
postgres adapter sub-packages and anything rooted under them:

- `github.com/pericles-luz/crm/internal/adapter/db/postgres` and any
  sub-package (`testpg`, future `migrations` helpers, etc.). This is the
  pool / tenant / connection seam.
- `github.com/pericles-luz/crm/internal/adapter/store/postgres` and any
  sub-package. Per-port store implementations live here
  (`idempotency_store`, `raw_event_store`, `tenant_association_store`,
  `webhook_token_store`, …). Each receives a `PgxConn` from the
  `db/postgres` pool and implements a domain-defined port (e.g.
  `webhook.IdempotencyStore`) without leaking pgx types upward.

Both paths are the seam where the hexagonal architecture _wants_ a SQL
driver to live. Everything else — `internal/iam`, `internal/inbox`,
`internal/ai`, `internal/webhook`, `internal/worker`, … — is on the
wrong side of the boundary and is rejected.

External test files for either adapter sub-package
(`package postgres_test`) share the allowlist — Go reports their import
path as `<adapter>_test`, and the analyzer strips that suffix before
checking.

## Override marker

If a single import legitimately needs to bypass the rule (perf-test
fixture, code-gen tooling, etc.), silence it with an annotated
`forbidimport:ok` comment on the same line as the import or on the line
directly above:

```go
import (
    // forbidimport:ok perf-test fixture only; never linked into prod
    _ "database/sql"
)
```

A bare marker without a justification (`// forbidimport:ok` alone) is
**not** honored. Reviewers should be able to read the reason for any
override directly in the diff.

## Wiring

| Surface           | Command                                                                    |
| ----------------- | -------------------------------------------------------------------------- |
| Local dev         | `make lint-imports`                                                        |
| CI                | `forbidimport analyzer (./internal/...)` step in `.github/workflows/ci.yml` |
| Self-test         | `go test ./tools/lint/forbidimport/... -count=1`                           |

The analyzer is built as a single `go vet` tool at
`bin/forbidimport`, mirroring the `notenant` analyzer's structure
(`tools/lint/notenant/`).

## Adding a new forbidden package

Edit `tools/lint/forbidimport/analyzer.go` — add the package to
`forbiddenExact` (for `==` matches) or `forbiddenPrefixes` (for any
sub-package match), then add a fixture line under
`testdata/src/badpkg/bad.go` with the matching `// want "..."` diagnostic.

## Adding a new allowed package

Be conservative — every entry weakens the boundary. If a new
`internal/adapter/...` adapter genuinely owns a different DB driver
(e.g. SQLite for offline tooling), or a new sibling postgres
sub-package emerges with the same hexagonal role as `db/postgres` and
`store/postgres`, add its import-path prefix to `allowedPkgPrefixes`
and document the rationale here under "Allowed paths".

## Why the rule

The forbidden imports are not banned because Go's stdlib is bad — they
are banned because every byte of `database/sql` outside the adapter is
one more place where the next bug lives. Tenant scoping
(`app.tenant_id`), RLS, audit logging, and the `WithTenant` /
`WithMasterOps` helpers all live in the adapter. A direct
`db.Exec` from `internal/inbox` would route around every one of them.

See also:

- ADR 0071 — Postgres roles + tenant scoping (`docs/adr/0071-postgres-roles.md`)
- SIN-62232 — `notenant` analyzer (forbids direct `*pgxpool.Pool` data
  methods even within the adapter package boundary)
- SIN-62216 — this rule
