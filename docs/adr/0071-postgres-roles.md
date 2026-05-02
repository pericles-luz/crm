# ADR 0071 — Postgres roles for the multi-tenant CRM

- Status: Accepted
- Date: 2026-05-02
- Owners: Coder (SIN-62232), CTO (review)
- Supersedes: —
- Related: ADR 0072 (RLS policies), SIN-62221 (security hardening epic),
  SIN-62220 #document-security-review (original SecurityEngineer finding)

## Context

The CRM is multi-tenant from day 1. Every tenant-bearing table includes a
`tenant_id uuid NOT NULL` column and is protected by Row Level Security
(see ADR 0072). RLS only protects what is read or written by a role for
which the policy applies — the policies in ADR 0072 are written `TO
app_runtime`. Anything connecting as a Postgres superuser, the table owner,
or any role with `BYPASSRLS=true` slips past the policies entirely.

We therefore need explicit, separated database roles with distinct postures
so that "the application" cannot accidentally use the credentials that are
allowed to bypass RLS, and so that cross-tenant operations have an
auditable trail.

## Decision

Create three Postgres roles. Their postures are encoded in
`migrations/0001_roles.up.sql`; this ADR is the design rationale.

### `app_runtime`

The default role used by the CRM application at runtime. Connection pool is
the production traffic path.

| Attribute     | Value          | Reason                                         |
|---------------|----------------|------------------------------------------------|
| `LOGIN`       | yes            | Used by the pool.                              |
| `SUPERUSER`   | no             | Defense in depth.                              |
| `CREATEDB`    | no             | Should never need it.                          |
| `CREATEROLE`  | no             | Should never need it.                          |
| `BYPASSRLS`   | **`NOBYPASSRLS` (false)** | **Load-bearing.** RLS policies in ADR 0072 are written `TO app_runtime`; if BYPASSRLS were on, every policy would be bypassed silently. |
| Append-only tables | `REVOKE UPDATE, DELETE` | `token_ledger`, `master_ops_audit`, future `audit_log`s. Append-only at the role level even when the policy would otherwise allow it. |

`app_runtime` is what `WithTenant` (in `internal/adapter/db/postgres`) uses
to start every tenanted transaction.

### `app_admin`

Used **only** at deploy time to run schema migrations. Not embedded in the
running app binary.

| Attribute     | Value           | Reason                                                                  |
|---------------|-----------------|-------------------------------------------------------------------------|
| `LOGIN`       | yes             | Migration runner connects as this role.                                 |
| `SUPERUSER`   | no              | Migrations don't need pg_authid edits or other superuser-only DDL.      |
| `CREATEDB`    | no              | Database creation is a bootstrap concern, done by superuser one time.   |
| `CREATEROLE`  | no              | Roles are created in `0001_roles.up.sql` by the cluster superuser only. |
| `BYPASSRLS`   | **`BYPASSRLS` (true)** | Migrations create policies, attach triggers, and seed reference data — all of which need BYPASSRLS. Acceptable because this role is short-lived. |

### `app_master_ops`

Used by the "master operational" console that occasionally needs to
read/write across tenants — incident response, billing rollups, GDPR
deletions orchestrated by support.

| Attribute     | Value                | Reason                                                                                 |
|---------------|----------------------|----------------------------------------------------------------------------------------|
| `LOGIN`       | yes                  | Console connects directly.                                                             |
| `SUPERUSER`   | no                   | Defense in depth.                                                                      |
| `CREATEDB`    | no                   | —                                                                                      |
| `CREATEROLE`  | no                   | —                                                                                      |
| `BYPASSRLS`   | **`BYPASSRLS` (true)** | Cross-tenant read/write is the whole point.                                            |
| Audit         | trigger-enforced     | Every INSERT/UPDATE/DELETE triggers `master_ops_audit_trigger`, which RAISES if `app.master_ops_actor_user_id` is unset. Without an audit row the transaction aborts. |

The audit guarantee is in `migrations/0002_master_ops_audit.up.sql`. The
trigger compares `current_user` to `'app_master_ops'`, so accidentally
running the same SQL via `app_admin` (also BYPASSRLS) does not leave
the audit table empty by mistake — but it also does not write to
master_ops_audit either, which is intentional: admin-initiated work goes
through the deploy runner's own log.

## Credential injection

| Role             | Where the password lives                              | Where it's read                            |
|------------------|-------------------------------------------------------|--------------------------------------------|
| `app_runtime`    | secret manager (AWS SM / Vault), env `CRM_DB_PASSWORD`| read by app on startup, never logged       |
| `app_admin`      | secret manager, env `CRM_MIGRATION_PASSWORD`          | read **only** by the migration runner; never bundled with the production binary |
| `app_master_ops` | secret manager, env `CRM_MASTER_OPS_PASSWORD`         | read **only** by the master-ops console pod; never deployed to runtime app pods |

The migration `0001_roles.up.sql` deliberately does not set passwords. Ops
runs `ALTER ROLE … PASSWORD '…'` from the secret-manager-injected env at
deploy time; the SQL never appears in the migration history or the image.

The application Helm chart / deploy pipeline must verify before each deploy:

1. `app_runtime` is reachable AND `BYPASSRLS=false` (`SELECT rolbypassrls
   FROM pg_roles WHERE rolname='app_runtime'`).
2. `app_admin` and `app_master_ops` credentials are NOT mounted in app pods.
3. `pg_hba.conf` denies inbound connections as the cluster superuser from
   the application network.

## Consequences

- New tables that hold tenant data must (a) have `tenant_id uuid NOT NULL`,
  (b) follow the four-policy template in ADR 0072, (c) `GRANT` only what
  `app_runtime` actually needs (typically `SELECT, INSERT` for append-only,
  `SELECT, INSERT, UPDATE, DELETE` for editable), and (d) be owned by
  `app_admin`.
- The migration runner is an extra moving piece in the deploy pipeline.
  Acceptable because it already exists in any non-trivial schema project.
- An ops mistake that grants `BYPASSRLS=true` on `app_runtime` becomes a
  silent disaster. Mitigation: a CI postdeploy check inspects pg_roles for
  posture drift and fails the rollout. (Tracked separately under the Phase 0
  Bootstrap epic, [SIN-62192](/SIN/issues/SIN-62192).)
- The custom `notenant` analyzer (`tools/lint/notenant`) rejects direct
  `*pgxpool.Pool.Exec/Query/QueryRow/SendBatch/CopyFrom` calls anywhere
  under `internal/`. Without it, a careless `pool.Exec("DELETE FROM
  customers")` from a use-case package would bypass `WithTenant` entirely.

## Rejected alternatives

### A) Single role with `BYPASSRLS=true`

Simplest. App connects as one role that bypasses RLS, "the application
takes responsibility for filtering by tenant_id."

- **Why rejected.** This is exactly the posture the SecurityEngineer
  flagged in [SIN-62220 #document-security-review](/SIN/issues/SIN-62220#document-security-review).
  A single missed `WHERE tenant_id = ?` (a refactor, a quick hotfix, a SQL
  injection) leaks the entire database. RLS exists specifically so the
  database is the last line of defense. Single-role with BYPASSRLS=true
  voids that defense.

### B) Two roles — runtime (`BYPASSRLS=false`) + admin (`BYPASSRLS=true`)

The "obvious" version: separate the migration role, but fold cross-tenant
operations under the admin role.

- **Why rejected.** Two reasons.
  1. The admin role then has TWO loads — DDL during deploys, and ad-hoc
     cross-tenant ops during incidents. Their access patterns and audit
     requirements differ. Conflating them means we either over-audit the
     migration runner (noise) or under-audit the master ops console
     (compliance gap).
  2. Compliance review for cross-tenant access wants a single,
     dedicated role tag in `master_ops_audit.actor_role` — easier to
     report on and easier to alert on. With two roles that's role-hacking;
     with three it is the natural design.

### C) Per-tenant roles with `SET ROLE` per request

`CREATE ROLE tenant_<id>` for every tenant; the app does `SET ROLE
tenant_<id>` at the start of each transaction; RLS policies are written
`TO PUBLIC USING (tenant_id = current_setting('jwt.tenant_id'))` or
similar.

- **Why rejected.** Doesn't scale: tens of thousands of pg_authid rows,
  pool reuse becomes harder, and onboarding/offboarding a tenant requires
  `CREATE ROLE` and `DROP ROLE` calls during the request path. Adds
  blast-radius (CREATEROLE somewhere) for a property RLS already gives us
  (filter by tenant_id GUC).

## How to validate this ADR is upheld

Run `make lint test` after every change. The integration test
`TestRolesArePostureCorrect` queries `pg_roles` for the three roles and
fails if any of `BYPASSRLS / CREATEDB / CREATEROLE / SUPERUSER` deviates
from the matrix above. The notenant analyzer flags any direct
`*pgxpool.Pool` use under `internal/`.
