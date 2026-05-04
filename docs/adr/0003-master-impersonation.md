# ADR-0003: Master impersonation via `X-Impersonate-Tenant` with mandatory audit

- Status: Accepted (2026-05-04)
- Owners: CTO (decision), Coder (record), SecurityEngineer (review)
- Related: [SIN-62190#document-plan](/SIN/issues/SIN-62190#document-plan) (decisions #10, #16), [SIN-62192](/SIN/issues/SIN-62192) (Fase 0 bootstrap), ADR-0001 (stack), ADR-0002 (RLS), ADR 0071 (Postgres roles), [SIN-62241](/SIN/issues/SIN-62241) (master grant cap)

## Context

Sindireceita support and operations staff need to act inside a tenant's data —
to reproduce a customer-reported bug, fix bad data, or assist during onboarding.
Asking the customer for a credential is unacceptable (LGPD, separation of
duties, accountability). Logging in with a per-tenant shared admin is no
better.

We need a way for a small set of internal users to act *as* a tenant they do
not belong to, with two non-negotiable properties:

1. The act of impersonating MUST be auditable — who, when, which tenant, which
   action — and the audit record MUST exist before the action takes effect.
2. The path MUST be off by default and gated by an explicit user attribute, so
   that a regular user (or a regular user's compromised session) cannot
   impersonate, no matter what they put in headers.

This is decisions #10 and #16 in [SIN-62190#document-plan](/SIN/issues/SIN-62190#document-plan).

The interaction with ADR-0002 is delicate. ADR-0002 says tenant isolation is
enforced by RLS keyed on `app.tenant_id`. Impersonation by definition wants the
runtime to operate as a *different* tenant than the user's own. The control
must therefore happen *before* the GUC is set, not by widening the policy.

## Decision

Master impersonation is a single, narrow code path with the following shape.

### Identity gate

- A user is eligible to impersonate iff `users.is_master = true` (column owned
  by the user table; `false` by default; granted by an audited admin action,
  see [SIN-62241](/SIN/issues/SIN-62241)).
- Eligibility is re-checked on every request. We never cache "this session is
  master" outside the request.

### Request gate

- A request that wants to operate against a different tenant carries the
  header `X-Impersonate-Tenant: <tenant-uuid>`.
- The impersonation middleware:
  1. Loads the authenticated user.
  2. If `is_master` is false, the header is ignored. The request proceeds as
     the user's own tenant. This is important: an attacker who only has a
     non-master session gains nothing by sending the header.
  3. If `is_master` is true and the header is present and parses, the
     middleware validates the target tenant exists and is not soft-deleted.
  4. The middleware writes an `audit_log` row (see below) **inside the same
     transaction that will set the GUC** but **before** any business logic
     runs.
  5. Only after the audit insert returns success does the request continue
     with `app.tenant_id` set to the target tenant.

### Audit record

- Stored in `audit_log` (or the existing `master_ops_audit`, depending on the
  current schema migration). Contains: actor user id, actor session id,
  source ip, target tenant id, http method + path, request id, timestamp,
  reason (free-text, optional, surfaced via header `X-Impersonate-Reason`).
- The audit insert uses a separate Postgres role (`audit_writer`) that has
  `BYPASSRLS = true` **for INSERT only**. The audit table revokes UPDATE and
  DELETE from every role at the SQL level. ADR 0071 owns the role definitions;
  this ADR enumerates the requirement.
- The audit insert and the GUC `SET LOCAL` happen in the same transaction.
  If the audit insert fails, the request is aborted with `503` and no
  business code runs. This guarantees: if the request had effects, an audit
  row exists.

### Defaults

- `is_master` defaults to false on user creation.
- The header is stripped at the edge for any request that did not arrive
  through the master interface (path or sub-host), so leaked headers from
  upstream proxies cannot accidentally enable impersonation.
- Impersonation never widens permissions inside the target tenant: the master
  user gets the same role profile inside the target tenant that the
  *original* tenant grants to its admin. We document this explicitly in the
  authz layer.

## Consequences

### Positive

- Support work is possible without sharing customer credentials.
- Every cross-tenant action is non-repudiable: the audit row is written
  before the action, in the same transaction, by a role that cannot rewrite
  history.
- The control surface is small: one middleware, one column, one table, one
  header. Easy to review, easy to test.
- A compromised non-master session is *not* an impersonation vector — the
  identity gate runs before the request gate.

### Negative

- A new attack surface (the header). Mitigated by:
  - the `is_master` gate (only a tiny, audited population can use it),
  - stripping the header at the edge unless the request came through the
    master path,
  - the audit record being a hard precondition (no audit, no action),
  - separate role for audit writes so the audit table cannot be tampered
    with by the runtime role.
- An additional Postgres role (`audit_writer`) to provision and rotate. ADR
  0071 carries this cost.
- The "audit before action" guarantee depends on the audit insert sharing a
  transaction with the request. We must keep an integration test that
  verifies aborting the audit insert aborts the request.

### Neutral

- The header name is namespace-clean (`X-Impersonate-Tenant`) and we will
  keep it stable. If we later add per-action impersonation (e.g., audit
  reasons enforced server-side), it is an additive change.
- Master impersonation does not change ADR-0002's three layers — it sits
  *between* layers 1 and 2, swapping the tenant id that layer 2 will use.

## Reversibility

Reversible at low cost: if we later replace the header with a session-bound
mechanism (e.g., a short-lived assumed-role token), the `is_master` column,
audit table, and authz contract remain useful. A future ADR can supersede the
*transport* (header vs. token) without touching the audit guarantee.

## Threat model — quick reference

- **Compromised non-master user**: cannot impersonate (header ignored).
- **Compromised master user**: every action is audited; impact bounded by the
  population of master users (kept small per [SIN-62241](/SIN/issues/SIN-62241))
  and by anomaly detection on `audit_log`.
- **Edge bypass**: header stripped at Caddy for non-master paths; reverse
  proxy config is part of the deploy review.
- **Audit table tampering**: runtime role has no UPDATE/DELETE on `audit_log`;
  `audit_writer` has only `INSERT`.
- **Race between audit and action**: audit insert and `SET LOCAL` run in the
  same transaction; a failed audit aborts the action.
