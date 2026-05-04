# ADR-0001: Stack base

- Status: Accepted (2026-05-04)
- Owners: CTO (decision), Coder (record)
- Related: [SIN-62190#document-plan](/SIN/issues/SIN-62190#document-plan), [SIN-62192](/SIN/issues/SIN-62192) (Fase 0 bootstrap), ADR-0002 (RLS), ADR-0003 (master impersonation)

## Context

Sindireceita CRM is being bootstrapped from scratch. We need to commit to a runtime stack
before anyone writes a use-case. The plan in [SIN-62190](/SIN/issues/SIN-62190#document-plan)
makes a few hard constraints clear:

- Multi-tenant from day one, with tenant isolation enforced in the database
  (see ADR-0002), which rules out datastores without strong row-level isolation
  primitives.
- HTTPS, mTLS-friendly fronting, and routing per host (decision #18 in the plan).
- Server-rendered HTML with progressive enhancement instead of an SPA
  (decision #5/§3 in the plan): smaller surface area, fewer moving parts,
  no client/server schema split.
- Boring, well-understood, easy-to-hire-for technologies. Every dependency we
  add is something we will operate at 03:00.
- Observability before optimisation: structured logs, traces, metrics from the
  first request (PR10 will wire it).

The first PR (skeleton, [SIN-62208](/SIN/issues/SIN-62208)) materialised this stack
in `compose.yml`, the `Makefile`, and the Go module skeleton. This ADR records the
*why*.

## Decision

We adopt the following stack as the default for the CRM. Anything else needs an ADR
that supersedes the relevant slice of this one.

### Language and runtime

- **Go 1.22+**. Static binaries, predictable performance, strong stdlib for HTTP
  and crypto, large idiomatic ecosystem, easy to onboard. We get goroutines and
  `context.Context` propagation natively, which the rest of the stack assumes.

### Datastores

- **Postgres 16** as the system of record. Reasons: row-level security
  (ADR-0002 depends on it), JSONB when we need it, predictable transactional
  semantics, mature operational story. Rejected: MongoDB — no first-class RLS,
  weaker transactional guarantees for our access patterns, and we would still
  need a relational database for billing and audit. The cost of running two
  datastores outweighs the schema-flexibility benefit.
- **Redis 7** for caching, idempotency keys, and rate limit counters. Boring,
  fast, ubiquitous. Not the system of record — anything in Redis must be
  reconstructible from Postgres.
- **MinIO** (S3 API) for object storage (uploads, exports, backups). Lets us
  develop locally against the same API we will hit in production.

### Messaging

- **NATS 2** with JetStream for asynchronous work and webhooks. Lightweight,
  single binary, durable streams when we need them. Rejected: Kafka (operational
  cost too high for our volume) and SQS (vendor lock and not available locally).

### HTTP layer

- **`net/http` from the standard library** + **chi router**. chi is a thin,
  idiomatic router on top of `net/http`: middleware as `func(http.Handler) http.Handler`,
  no global state, no DSL. Rejected: Echo and Gin — both wrap the request/response
  in their own types, which leaks into handlers and use-cases and fights the
  hexagonal layout we want (see ADR-0002 and the architecture rules in
  [SIN-62192](/SIN/issues/SIN-62192)).
- **Caddy 2** as the edge: TLS termination, automatic certificates, and
  per-host routing (`X-Forwarded-Host` → tenant). Rejected: Nginx — works, but
  Caddy 2 gives us ACME and per-tenant TLS without extra glue.

### Frontend

- **Server-rendered HTML via `templ`** + **HTMX** for partial updates. This is
  decision #5/§3 of the plan. Reasons:
  - One language, one repo, one deploy artifact.
  - No client/server schema duplication; the server already has the data.
  - Progressive enhancement: every endpoint must produce a useful response without
    JavaScript, then HTMX layers `hx-*` swaps on top.
  - We are a small team. An SPA framework adds an entire second runtime to
    operate, version, and secure.
  - When we genuinely need client-side behaviour, we reach for small, pinned
    JavaScript modules — never a framework — and only with explicit ADR.
- Rejected: React/Vue/Svelte SPA. We are not building an offline-first product.

### Database access and migrations

- **`pgx/v5`** for Postgres. Native protocol, prepared statement cache,
  context-aware, plays well with `SET LOCAL` (ADR-0002 depends on this).
  Rejected: `database/sql` + `lib/pq` — older, no native protocol, no first-class
  support for `pgx.Tx` semantics we want.
- **goose** for migrations. Plain SQL files with up/down, runs from CI and the
  Makefile, no ORM coupling. Rejected: golang-migrate (works, but less ergonomic
  in our flow); ORMs (we want SQL to be SQL).

### Observability

- **`log/slog`** for structured logs (stdlib, no third-party logger).
- **OpenTelemetry SDK** for traces and metrics, exporting OTLP. Backends are a
  deployment concern (Jaeger/Tempo, Prometheus). Vendor-neutral by construction.
- **Prometheus** scrape endpoint for metrics, both runtime (`go_*`) and
  application (`http_requests_total`, etc.). Wired in PR10.

### Tooling

- `go vet`, `staticcheck`, `goimports` are mandatory in CI.
- Local lints (PR9) layer the project-specific rules on top.

## Consequences

### Positive

- Single language for backend and templates, single deploy artifact, fast builds.
- Postgres carries both the data and the tenancy boundary — fewer places to
  audit for cross-tenant leaks (see ADR-0002).
- Operating five processes (Postgres, Redis, NATS, MinIO, Caddy) is well within
  what one operator can keep in their head.
- HTMX-first frontend keeps the team small and the surface area auditable.

### Negative

- Vendor lock to Postgres: ADR-0002 depends on PG-specific features (RLS,
  `SET LOCAL`). Migrating off Postgres would be expensive. We accept this; it
  is a deliberate trade-off for the security posture in ADR-0002.
- HTMX-first means rich client UX (drag-and-drop, complex offline forms) needs
  bespoke JavaScript. We pay this cost case by case.
- NATS is less ubiquitous than Kafka — knowledge transfer when hiring needs
  attention.

### Neutral

- Caddy auto-TLS requires the host to reach Let's Encrypt during cert renewal.
  Internal-only deployments need a TLS configuration ADR before they exist.
- We do not commit to a frontend CSS framework here; that is a separate
  decision tracked under the UX track.

## Reversibility

Append-only. A future ADR can mark sections of this one as `Superseded` if a
specific component is replaced — for example, "ADR-00xx supersedes ADR-0001 §
Messaging" if we ever swap NATS. The rest of ADR-0001 stays in force.
