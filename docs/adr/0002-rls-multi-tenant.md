# ADR-0002: Multi-tenant isolation via Postgres RLS + `app.tenant_id`

- Status: Accepted (2026-05-04)
- Owners: CTO (decision), Coder (record), SecurityEngineer (review)
- Related: [SIN-62190#document-plan](/SIN/issues/SIN-62190#document-plan), [SIN-62192](/SIN/issues/SIN-62192) (Fase 0 bootstrap), ADR-0001 (stack), ADR-0003 (master impersonation), ADR 0071 (Postgres roles), ADR 0072 (RLS policies)

## Context

The CRM serves many tenants from a single Postgres instance. A single
mistakenly-omitted `WHERE tenant_id = ?` clause is a cross-tenant data leak.
Code review and tests catch most slips, but not all — and the cost of a leak is
catastrophic (credibility, LGPD, contractual).

We want isolation that survives:

- A use-case author who forgets to thread `tenantID` into a query.
- A new team member who copies an existing query without understanding the
  tenancy guard.
- A future ORM helper or report that runs raw SQL.
- A debugging session where someone connects with the runtime role and pokes
  around.

In other words: isolation must hold even when the application is wrong.

The constraint comes from [SIN-62190#document-plan](/SIN/issues/SIN-62190#document-plan)
(decisions #4, #6, and the security cross-cuts). ADR-0001 already locks Postgres
in as the system of record, which makes Postgres-native isolation primitives
the obvious mechanism.

Alternative considered and rejected: **filter in code only**. A single helper
that auto-injects `tenant_id` into every query. The failure mode is silent:
the day someone bypasses the helper (a one-off SQL, a new package, a
COPY-paste from main), the leak is invisible until exploited. We reject this
as the primary control. We may still keep query helpers as a *convenience*,
but they are not the security boundary.

## Decision

Tenant isolation is enforced in **three layers**. All three must be in place
for any tenanted table.

### Layer 1 — HTTP middleware (boundary)

The HTTP edge resolves the tenant from the request (host header → tenant id,
or master impersonation per ADR-0003), validates that the authenticated user
belongs to that tenant, and stores the tenant id in the request context.
Handlers receive only `ctx`; they never read the host header directly.

### Layer 2 — Transaction-scoped GUC

Every database operation runs inside a transaction opened by a single helper
(`WithTenant(ctx, db, fn)` in `internal/adapter/db/postgres`). The helper:

1. Begins a transaction on the `app_runtime` role (see ADR 0071).
2. Issues `SET LOCAL app.tenant_id = $1` with the tenant id from `ctx`.
3. Calls the caller's function with the transaction.
4. Commits or rolls back.

`SET LOCAL` ties the GUC to the transaction, so it cannot leak to the next
checkout from the connection pool. Use-cases never call `SET` directly.
Use-cases that legitimately need to operate without a tenant (cron jobs,
master-mode reads — see ADR-0003) call a different, explicit helper that
shows up in code review.

### Layer 3 — Database row-level security

Every table that holds tenant-bearing data:

- has a `tenant_id uuid NOT NULL` column,
- has `ENABLE ROW LEVEL SECURITY` and `FORCE ROW LEVEL SECURITY`,
- has a policy `TO app_runtime` of the form
  `USING (tenant_id = current_setting('app.tenant_id')::uuid)
   WITH CHECK (tenant_id = current_setting('app.tenant_id')::uuid)`.

Concrete role definitions live in ADR 0071; policy templates live in ADR 0072.
The point of *this* ADR is to record that RLS is the load-bearing control,
not a belt-and-braces extra.

### What we do not do

- We do not rely on `SECURITY DEFINER` functions to filter rows.
- We do not give the runtime role `BYPASSRLS`. ADR 0071 enforces this.
- We do not let migrations or one-off scripts run as the runtime role; they
  use a separate role (`app_admin`).

## Consequences

### Positive

- A query that forgets `WHERE tenant_id = ?` returns zero rows for the wrong
  tenant instead of leaking. RLS is the safety net.
- Defence in depth: a leak requires bypassing all three layers.
- Auditability: `current_setting('app.tenant_id')` is observable in pg
  logs and OTel spans, so we can confirm at runtime which tenant a query
  ran under.
- Use-case code stays clean: it reads `ctx`, not headers or session.

### Negative

- Every tenanted query pays for a `SET LOCAL` round-trip. In practice this is
  a few microseconds and is amortised across the transaction's real work,
  but high-fanout read paths must batch operations into one transaction.
- Vendor lock-in to Postgres deepens. We accept this — ADR-0001 already
  commits to Postgres, and the security posture is worth more than database
  portability.
- Developers must learn the `WithTenant` helper and resist the temptation to
  bypass it. CI lints (PR9) will catch direct calls to `db.Query` outside
  the helper.
- A bug in the policy itself is now a single point of failure. We mitigate
  this by:
  - keeping policies short and uniform (ADR 0072),
  - covering them with integration tests that boot a real Postgres and
    verify cross-tenant reads return zero rows,
  - reviewing every migration that touches a policy with the SecurityEngineer.

### Neutral

- Tools that connect with `app_admin` (e.g., `psql` for emergencies) see all
  rows. This is intentional and audited via session logging.
- Read replicas inherit policies as long as they are reached through a role
  without `BYPASSRLS`.

## Reversibility

This ADR is hard to reverse: removing RLS would mean accepting a single failure
mode (forgotten `WHERE`) as catastrophic. A future ADR could extend the model
(e.g., add a fourth layer, or move some tenanted tables to per-tenant schemas)
but the three layers here are the floor, not the ceiling.
