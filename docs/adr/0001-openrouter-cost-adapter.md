# ADR 0001 — OpenRouter cost-API adapter

- Status: Accepted
- Date: 2026-05-02
- Issue: SIN-62268 (follow-up of SIN-62240, wallet F37)

## Context

The wallet reconciliator (`internal/wallet/usecase/reconciliator.go`) needs a fourth drift layer
that compares Sindireceita's internal ledger against the upstream
OpenRouter cost API once per day. The port `port.OpenRouterCostAPI`
and the inline drift loop landed with SIN-62240; this ADR pins the
contract for the production HTTP adapter at
`internal/wallet/adapter/openrouter/http.go`.

## Decision

### Endpoint version (pinned)

- Base URL: `https://openrouter.ai`
- Path: `/api/v1/credits/daily`
- Method: `GET`
- Query parameters:
  - `master_id` — Sindireceita master identifier (string).
  - `date` — UTC day in `YYYY-MM-DD` (the adapter normalises to UTC start-of-day before sending).
- Auth: `Authorization: Bearer ${OPENROUTER_API_KEY}` — secret is loaded from env in `cmd/walletreconciler`.

Response (pinned shape):

```json
{
  "data": {
    "master_id": "master-abc",
    "date": "2026-05-01",
    "total_tokens": 12345,
    "cost_usd": 1.234567
  }
}
```

A change to this shape upstream MUST bump the ADR (`0002-…`) and
update `dailyUsageResponse` in the adapter at the same time. The
adapter caps response bodies at 1 MiB.

### Conversion factor

The wallet's canonical unit is `token`. OpenRouter reports tokens
directly in `data.total_tokens`, and the conversion is **1:1
pass-through** — we do **not** rederive tokens from `cost_usd`. The
`cost_usd` field is captured but not used: it is kept so a future
auditor can cross-check against OpenRouter's billing portal without a
schema change.

If OpenRouter ever stops reporting `total_tokens`, we will need a new
ADR to define the USD→tokens factor (probably the per-master pricing
table from D4 in SIN-62207).

### Error classification

The adapter returns:

- Plain `error` from network / decode / unexpected status (default
  case — surface to the cron and fail the master).
- `errors.Is(err, openrouter.ErrAuth)` for 401 / 403 — operator MUST
  rotate the API key.
- `errors.Is(err, openrouter.ErrRateLimit)` for 429 — concrete type
  is `*openrouter.RateLimitError` carrying `RetryAfter`. The adapter
  honours `Retry-After` (delta-seconds or HTTP-date) and retries up to
  `DefaultMaxRetries` (2) before surfacing.
- 5xx upstream is treated as retryable, also bounded by
  `DefaultMaxRetries`.

### Routine wiring

`cmd/walletreconciler` reads `OPENROUTER_API_KEY` at startup. When
set, `buildOpenRouter()` constructs the adapter; otherwise it returns
`nil` and the reconciliator skips the inline drift loop. Optional
`OPENROUTER_BASE_URL` overrides the host for stg / smoke tests.

The cron schedule (Paperclip routine, daily 03:00 UTC) lives outside
this repo. Rolling back the feature is two steps:

1. Disable / remove the `OPENROUTER_API_KEY` secret in the deploy
   environment (the binary self-detects and skips the drift loop).
2. Optionally pause the routine entry from the Paperclip UI.

## API key rotation procedure

1. **Provision** a new key in the OpenRouter dashboard
   (`https://openrouter.ai/keys`) with the same scopes as the
   outgoing key (`read:credits`).
2. **Stage** the new key alongside the old one in the secret store
   (e.g. `OPENROUTER_API_KEY_NEW`) — do **not** overwrite the active
   key yet.
3. **Smoke-test** by running `walletreconciler` with
   `OPENROUTER_API_KEY=$OPENROUTER_API_KEY_NEW` against stg — confirm
   the run completes without `ErrAuth` and the
   `wallet_openrouter_drift_pct` gauge updates for at least one
   master.
4. **Promote**: rename `OPENROUTER_API_KEY_NEW` → `OPENROUTER_API_KEY`
   in production. The next nightly run picks it up automatically.
5. **Revoke** the old key from the OpenRouter dashboard.
6. **Audit**: verify the post-rotation run emitted no
   `wallet.openrouter_drift` alerts that did not exist pre-rotation.

If a leaked key is suspected, skip steps 2–4: revoke immediately and
disable the drift loop until a fresh key is provisioned (the
reconciliator already tolerates `OPENROUTER_API_KEY` being unset).

## Consequences

- Upstream API drift is now monitored daily; alerts fire at >5%
  divergence (`DefaultOpenRouterDriftAlertPct`).
- The adapter is the only place we encode the OpenRouter URL shape;
  changes are reversible by rolling back two files
  (`http.go`, `cmd/walletreconciler/main.go`).
- The 1:1 token pass-through assumes OpenRouter keeps reporting
  `total_tokens`. A schema change there is the most likely failure
  mode and is monitored implicitly: a missing field decodes to `0`
  tokens, which produces a 100% drift alert on the next run.
