# `webhook-token-mint` — operator CLI for `webhook_tokens`

Out-of-band tool that mints and rotates webhook tokens for the
`/webhooks/{channel}/{plaintext}` route described in
[ADR 0075 §2 D1](../../docs/adr/0075-webhook-security.md). Issue
[SIN-62278](/SIN/issues/SIN-62278) covers the rationale (review gap on
[SIN-62234](/SIN/issues/SIN-62234)).

> **Why it exists.** The `webhook_tokens` table stores
> `sha256(plaintext)` only. Without this CLI, an operator has no
> sanctioned way to put rows in that table, and the production feature
> flag `webhook.security_v2.enabled = on` would drop every authentic
> Meta callback as `unknown_token`.

## Build

```bash
go build -o ./bin/webhook-token-mint ./cmd/webhook-token-mint
```

The binary depends on a reachable Postgres (the same `webhook_tokens`
schema that `cmd/server` uses).

## Connection string

The CLI takes the connection string from `--dsn` (highest priority)
or the `DATABASE_URL` env var. Treat the DSN as a credential — never
inline it on a shared shell.

```bash
export DATABASE_URL='postgres://crm@db.internal/crm?sslmode=require'
```

## Mint a fresh token

```bash
./bin/webhook-token-mint \
    --channel whatsapp \
    --tenant-id 11111111-2222-3333-4444-555555555555 \
    --overlap-minutes 5      # stored as metadata; no rotation happens for an initial mint
```

Sample stdout (the plaintext is the only thing the operator must
capture; everything else is reproducible from the DB row):

```
===== webhook-token-mint =====
channel:     whatsapp
tenant_id:   11111111-2222-3333-4444-555555555555
hash (hex):  3c…<sha256>
rotation:    none (initial mint, overlap_minutes stored as 5)

TOKEN PLAINTEXT (copy this NOW — it cannot be retrieved later):
8b3a…<64 hex chars>

webhook URL: /webhooks/whatsapp/8b3a…
```

> **Treat stdout like a secret.** The plaintext appears exactly once.
> Pipe stdout to a file and remove from shell history if the host is
> shared. The DB stores `sha256(plaintext)`; nobody (including the CLI
> author) can recover the plaintext from the row after the fact.

The provisioning flow with a Meta operator is:

1. Run `webhook-token-mint` and capture the plaintext.
2. Hand the plaintext to the Meta operator via your secret-sharing
   channel of record.
3. The operator configures the Meta app webhook URL as
   `https://<your-host>/webhooks/whatsapp/<plaintext>`.
4. Trigger a test event from Meta's UI; confirm the
   `webhook_received_total{channel="whatsapp",outcome="accepted"}`
   counter ticks up
   (see [SIN-62275](/SIN/issues/SIN-62275) for the dashboard).

## Rotate a token

Rotation is **mint a new active row + schedule the old row's
`revoked_at`**. During the overlap window both tokens resolve to the
same tenant, so the Meta operator can swap the URL on the Meta side
without dropping a single message.

```bash
./bin/webhook-token-mint \
    --channel whatsapp \
    --tenant-id 11111111-2222-3333-4444-555555555555 \
    --overlap-minutes 5 \
    --rotate-from-token-hash-hex 3c…<sha256-of-old-plaintext>
```

The `<sha256-of-old-plaintext>` value is the `hash (hex)` line printed
by the previous mint. It is also queryable from
`webhook_tokens.token_hash`:

```sql
SELECT encode(token_hash, 'hex'), created_at, revoked_at
  FROM webhook_tokens
 WHERE channel = 'whatsapp'
   AND tenant_id = '11111111-2222-3333-4444-555555555555'
   AND revoked_at IS NULL;
```

Rotation semantics (matches ADR 0075 rev 3 §2 D1 / F-13):

- `--overlap-minutes 0` is an immediate cut: the old token stops
  resolving the instant the new one is minted.
- `--overlap-minutes N` (N > 0) sets `revoked_at = now() + N min` on
  the old row. Both rows are valid until the timestamp expires.
- A unique index over `(channel, token_hash) WHERE revoked_at IS NULL`
  prevents two simultaneously-active tokens with the same hash;
  collisions on 256-bit entropy are astronomical, so this only fires
  on copy-paste mistakes.

## Failure modes

| Symptom on stderr                                                 | What it means                                         | Operator action                                                                                                                |
|-------------------------------------------------------------------|-------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------|
| `--dsn or env DATABASE_URL is required`                           | Neither flag nor env var set.                         | Set one and retry.                                                                                                              |
| `active token with this hash already exists for channel "..."`    | Partial-unique-index conflict on insert.              | Retry — `crypto/rand` will produce a different plaintext.                                                                       |
| `webhook: no active token for (channel, token_hash)` (rotation)   | The `--rotate-from-token-hash-hex` did not match any active row. | Re-derive the hash from the old plaintext or query `webhook_tokens` directly.                                                   |
| `WARNING: new token row was inserted but the old token's revocation could not be scheduled.` | New row landed; revoke step failed mid-rotation. | Run the printed `DELETE` statement to remove the new row, then retry the rotation. Until you do, BOTH tokens resolve to tenant. |

The CLI exits **non-zero** on any error, and **never prints the
plaintext line on a failure path** (because no row was created).

## Security notes (for review)

- Token entropy: `crypto/rand.Read(make([]byte, 32))` — 256-bit
  CSPRNG, hex-encoded for URL safety. `math/rand` is forbidden in
  `internal/webhook/` and any `*gen.go` file by the
  `paperclip-lint nomathrand` analyzer (F-10).
- Plaintext is never persisted, never logged, never put in a metric
  label, and only ever returned to stdout once.
- The CLI's database identity should be a dedicated role with
  `INSERT, UPDATE` on `webhook_tokens` only — not the same role the
  webhook handler uses (which only needs `SELECT, UPDATE last_used_at`).

## Tests

```bash
go test -cover ./cmd/webhook-token-mint
go test -cover ./internal/webhook
go test -cover ./internal/adapter/store/postgres
```

Coverage targets per Sindireceita's quality bar are ≥ 85 % per
package; the SIN-62278 commit landed at 88.3 / 96.2 / 96.7 %
respectively.
