# Architecture Decision Records

Architecture Decision Records (ADRs) capture the *why* behind load-bearing
choices. The code shows what we did; an ADR records the constraint we were
under, what we decided, and what we accepted in exchange.

We use a trimmed Michael Nygard format. Every ADR has these four sections:

- **Status** — `Proposed`, `Accepted`, `Superseded by ADR-XXXX`, or
  `Rejected`. Include the date.
- **Context** — what problem we are solving and what constraints apply.
- **Decision** — what we are doing.
- **Consequences** — what changes because of the decision (positive,
  negative, neutral).

## Index

| ADR                                              | Title                                                              | Status   |
| ------------------------------------------------ | ------------------------------------------------------------------ | -------- |
| [0001](./0001-stack-base.md)                     | Stack base (Go, Postgres, Redis, NATS, MinIO, Caddy, chi, HTMX)    | Accepted |
| [0002](./0002-rls-multi-tenant.md)               | Multi-tenant isolation via Postgres RLS + `app.tenant_id`          | Accepted |
| [0003](./0003-master-impersonation.md)           | Master impersonation via `X-Impersonate-Tenant` with mandatory audit | Accepted |
| [0071](./0071-postgres-roles.md)                 | Postgres roles for the multi-tenant CRM                            | Accepted |
| [0072](./0072-rls-policies.md)                   | Row-level security policies                                        | Accepted |

## Authoring rules

- Filenames use the pattern `NNNN-kebab-title.md`. Numbers are append-only
  and never reused.
- ADRs are immutable once accepted. To revise a decision, write a new ADR
  whose Status is `Accepted` and which marks the prior ADR as
  `Superseded by ADR-NNNN` (and update the prior ADR's Status line in the
  same PR).
- Keep an ADR under ~200 lines. If it grows, split it.
- Link to the source decision (issue, plan document) and to neighbouring
  ADRs from the header. Use the company-prefixed link form
  (`/SIN/issues/SIN-XXXXX`) so the references stay clickable inside
  Paperclip.

## Where to add the next one

The next ADR file is `NNNN-<title>.md` where `NNNN` is the next free number
(check the index above). Add the row to the table in this README in the same
commit that introduces the ADR.
