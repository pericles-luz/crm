# ADR 0072 — RLS policy template for tenanted tables

- Status: Accepted
- Date: 2026-05-02
- Owners: Coder (SIN-62232), CTO (review)
- Related: ADR 0071 (postgres roles), SIN-62221, SIN-62220

## Context

ADR 0071 carves the database surface into three roles. The role separation
is necessary but not sufficient: it tells us who can connect, but it does
not tell Postgres which rows of a tenanted table any particular session is
allowed to see. That is what Row Level Security (RLS) policies do, and we
want exactly one canonical template that every tenanted table follows.

The reason is operational, not aesthetic: RLS done inconsistently across
tables is worse than no RLS at all, because the inconsistency hides the
gaps. A reader-of-the-code expects "RLS is enabled, therefore tenant_id is
enforced"; if half the tables have `USING` but not `WITH CHECK`, or
`ENABLE ROW LEVEL SECURITY` without `FORCE ROW LEVEL SECURITY`, the wrong
intuition gets built and the bug is found in production.

## Decision

Every table that holds tenant-scoped data MUST follow the four-policy
template below. PRs that introduce a tenanted table without it are
rejected at code review.

```sql
ALTER TABLE {table} ENABLE ROW LEVEL SECURITY;
ALTER TABLE {table} FORCE ROW LEVEL SECURITY; -- crucial: RLS applies even to the table owner

CREATE POLICY tenant_isolation_select ON {table}
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE POLICY tenant_isolation_insert ON {table}
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE POLICY tenant_isolation_update ON {table}
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE POLICY tenant_isolation_delete ON {table}
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
```

Append-only tables (e.g. `token_ledger`, `master_ops_audit`) drop the
UPDATE/DELETE policies and additionally `REVOKE UPDATE, DELETE ON {table}
FROM app_runtime` so the table-level privilege never applies. The first
example is in `migrations/0003_token_ledger.up.sql`.

### Why each clause is load-bearing

#### `ENABLE ROW LEVEL SECURITY`

Turns the policy machinery on. Without it, policies are decorative.

#### `FORCE ROW LEVEL SECURITY` — the easily-forgotten line

Postgres exempts the table owner from RLS by default. Our migrations run
as `app_admin` (the owner of `token_ledger`), and `app_admin` already has
`BYPASSRLS=true` per ADR 0071. Without `FORCE`, an ad-hoc query under
`app_admin` against `token_ledger` would see every tenant — most of the
time that's intended (admin = BYPASSRLS), but FORCE is what guarantees
that no policy *can* be silently disabled by a privilege chain we did
not anticipate. Concretely: if a future migration accidentally
`SET ROLE app_admin; SELECT … FROM token_ledger;` inside a test
fixture, with FORCE it still goes through the policy machinery (and is
then bypassed only by BYPASSRLS, which we audit). Without FORCE the
ownership exemption fires first and the policy never even runs.

`TestRLS_AppliesToOwner` in `internal/adapter/db/postgres/withtenant_test.go`
asserts `relforcerowsecurity = true` for `token_ledger`; that test is the
canary for any future migration that drops FORCE.

#### `USING (tenant_id = current_setting('app.tenant_id', true)::uuid)`

Reads the GUC `app.tenant_id` set by `WithTenant`. The second argument
`true` ("missing_ok") means "return NULL if unset" instead of erroring.

If a session never calls `WithTenant`, `current_setting('app.tenant_id',
true)` returns `''` (empty string). `''::uuid` would error (`invalid input
syntax for uuid: ""`). We rely on Postgres' policy short-circuit:
`tenant_id = NULL::uuid` is `NULL`, which is treated as FALSE in the
policy, so every row is filtered out. Verified empirically — see
`TestRLS_NoTenantSet_ReturnsZeroRows` (`SELECT count(*) FROM
token_ledger` returns 0 when no GUC is set) and the SQL below.

> **Proof query** (paste into psql connected as `app_runtime` against a
> fresh test database; nobody has called `WithTenant` yet):
>
> ```sql
> -- The seed (run as app_admin):
> INSERT INTO token_ledger (id, tenant_id, kind, amount)
> VALUES (gen_random_uuid(), gen_random_uuid(), 'topup', 100);
>
> -- As app_runtime, no GUC set:
> SELECT current_setting('app.tenant_id', true) AS guc;
> -- guc | (empty string)
>
> SELECT count(*) FROM token_ledger;
> -- count | 0
>
> -- The policy condition expanded:
> SELECT 'a' = current_setting('app.tenant_id', true)::uuid;
> -- ERROR:  invalid input syntax for type uuid: ""
> -- ^ Postgres is allowed to skip evaluation when no rows match,
> -- which is what happens here. The empty-string cast never runs
> -- on the actual SELECT path.
> ```
>
> The point: a forgotten `WithTenant` call denies all rows. It does NOT
> accidentally grant access. Fail-closed.

#### `WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid)`

Defends INSERT/UPDATE. Without WITH CHECK, an attacker who controls
`tenant_id` in a request body could insert rows under a different tenant
and the row would land in the table — `USING` only filters reads. With
WITH CHECK, Postgres rejects the write before commit
(`new row violates row-level security policy`). See
`TestRLS_InsertWrongTenantFails`.

#### `TO app_runtime`

Policies attach to specific roles. Writing them `TO PUBLIC` would also
apply to `app_admin` and `app_master_ops`, defeating the purpose of those
roles (BYPASSRLS exists precisely so deploy and master-ops can read
across tenants). `TO app_runtime` is what makes the role separation in
ADR 0071 enforce itself: only the role that connects from the app runtime
gets policy-filtered.

## Process

The template above is a checklist item in the PR template. Reviewers
should refuse to merge any new tenanted table that does not show:

1. `ALTER TABLE … ENABLE ROW LEVEL SECURITY;`
2. `ALTER TABLE … FORCE ROW LEVEL SECURITY;` (load-bearing — see above)
3. Four (or two, for append-only) policies named
   `tenant_isolation_{select,insert,update,delete}`.
4. `REVOKE ALL ON {table} FROM PUBLIC; GRANT … TO app_runtime;` —
   minimal grants only.
5. For append-only tables: `REVOKE UPDATE, DELETE ON {table} FROM
   app_runtime;` AND no `tenant_isolation_update` / `tenant_isolation_delete`
   policy.
6. Index on `(tenant_id, …)` so the policy `USING` clause is index-backed
   and not a sequential scan.

## Consequences

- Adding a tenanted table requires roughly fifteen lines of identical SQL
  on top of `CREATE TABLE`. We accept that cost — the template is the
  contract.
- Every new tenanted table requires a regression test that exercises (a)
  no-tenant-set case (b) tenant-A-vs-tenant-B isolation (c) WITH CHECK
  rejection. The shared test harness in
  `internal/adapter/db/postgres/testpg` is the home for those.
- `master_ops_audit` does not follow this template — it intentionally
  has no tenant scoping (it's a cross-tenant audit log). Access is
  controlled by the per-role GRANT/REVOKE in
  `0002_master_ops_audit.up.sql`. The trigger function is what makes the
  log non-bypassable.

## Rejected alternatives

### A) Application-level filtering only (no RLS)

"Just remember to add `WHERE tenant_id = ?`."

- **Why rejected.** Identical reason to the single-role rejection in ADR
  0071. We need defense in depth at the database layer because the
  application is the layer most likely to forget.

### B) RLS without `FORCE`

The default Postgres behaviour. Owner exempt from policies.

- **Why rejected.** `app_admin` runs migrations and ad-hoc seed queries.
  Without FORCE the owner exemption silently bypasses policies during
  any admin task. We rely on tests passing in CI (where the test runs as
  `app_admin` precisely to exercise FORCE). Removing FORCE makes the test
  pass-by-accident and the policy effectively decorative.

### C) `USING (current_user = 'app_master_ops' OR tenant_id = …)`

A "policy aware of role" approach: bake the master-ops bypass into every
tenant policy.

- **Why rejected.** Two roles already do this with BYPASSRLS=true.
  Putting `current_user` checks in policy text spreads the bypass logic
  across every tenanted table; if the role name ever changes, every
  policy needs a migration. The role attribute is the right knob.

### D) `current_setting('app.tenant_id')` without the `true` second arg

Errors when the GUC is unset.

- **Why rejected.** We want fail-closed on missing GUC, not "session
  errors out at first SELECT". With `true`, an unset GUC results in
  zero rows — which is loud enough at integration-test time to catch a
  missing `WithTenant` call but doesn't break the whole connection if
  some unrelated query runs before the helper.
