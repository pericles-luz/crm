# Multi-tenant RLS — operator cheat sheet (SIN-62209)

This is the short reference for "what does RLS do in this codebase, and
how do I not break it?". The full design lives in two ADRs:

- [ADR 0071 — Postgres roles](../adr/0071-postgres-roles.md)
- [ADR 0072 — RLS policy template](../adr/0072-rls-policies.md)

If anything below disagrees with an ADR, the ADR wins.

## Three roles, three postures

| Role             | `BYPASSRLS` | Used by                                       |
|------------------|-------------|-----------------------------------------------|
| `app_runtime`    | **NO**      | The CRM application at request time.          |
| `app_admin`      | yes         | `migrate-up` / `migrate-down` only.           |
| `app_master_ops` | yes         | Cross-tenant ops console; every write audited.|

`app_runtime` is the only role that the CRM binary opens connections as.
RLS is the third defense layer (after auth and middleware): even if
middleware forgets to call `WithTenant`, the policies filter every row.

## The four-policy template

Every tenanted table follows the canonical `tenant_isolation_{select,insert,update,delete}`
template from ADR 0072 and additionally turns on `FORCE ROW LEVEL
SECURITY` so the table owner (`app_admin`) is not silently exempted.
Append-only tables (`audit_log`, `master_ops_audit`, `token_ledger`) keep
only the SELECT and INSERT policies and add table-level `REVOKE UPDATE,
DELETE` for double-locking.

This PR adds four tables that follow the template:

- `tenants` — tenant registry. **Not** tenant-scoped (it has no
  `tenant_id`); `app_runtime` only gets `SELECT` for host-to-tenant lookup.
- `users`, `sessions`, `audit_log` — full template, per ADR 0072.

## The master user exception

Master users (`is_master = true`, `tenant_id IS NULL`) are an explicit
exception to the "tenant_id NOT NULL" invariant in `users`. They are
**invisible** to `app_runtime`: the SELECT policy compares `tenant_id =
current_setting('app.tenant_id')::uuid`, which can never match a NULL
column, so masters drop out of every runtime query. They are only
reachable through `app_master_ops` (BYPASSRLS), and every write to
`users` under `app_master_ops` is captured by the
`master_ops_audit_trigger` from `0002_master_ops_audit`.

A `CHECK (users_master_xor_tenant)` constraint on `users` forces the
shape: `(is_master, tenant_id)` is either `(true, NULL)` or
`(false, <uuid>)`. There is no third state, so a regular tenant user
cannot accidentally land with `tenant_id = NULL` and become invisible.

## Quick verification recipe

Once `make migrate-up && make seed-stg` is green, the following session
proves criteria #3/#4 of SIN-62209:

```sql
\c crm app_runtime
-- No GUC set: every tenanted query returns 0 rows.
SELECT count(*) FROM users;             -- 0
SELECT count(*) FROM sessions;          -- 0
SELECT count(*) FROM audit_log;         -- 0

-- Scope to acme; master user (NULL tenant_id) stays hidden.
SET LOCAL app.tenant_id = '00000000-0000-0000-0000-00000000ac01';
SELECT email FROM users;                -- only acme agents
SELECT email FROM users
  WHERE tenant_id = '00000000-0000-0000-0000-00000000eb02';
                                        -- 0 rows (silent, not an error)
```
