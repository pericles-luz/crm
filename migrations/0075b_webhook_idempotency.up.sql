-- 0075b_webhook_idempotency.up.sql — SIN-62234 / ADR 0075 §3 (D2).
-- Replay-resistant dedup. NOT partitioned — global B-tree, lookup O(1).
-- Independent retention from raw_event; GC noturno via cron app-side.
--
-- idempotency_key = sha256(tenant_id || ':' || channel || ':' || raw_payload).
-- Justificativa de não usar length-prefix em §3 do ADR: tenant_id é UUID-fixed,
-- channel é enum-fechado [a-z0-9_]+, último ':' delimita unambiguamente.

CREATE TABLE IF NOT EXISTS webhook_idempotency (
    tenant_id       uuid        NOT NULL,
    channel         text        NOT NULL,
    idempotency_key bytea       NOT NULL,
    inserted_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, channel, idempotency_key)
);

CREATE INDEX IF NOT EXISTS webhook_idempotency_gc_idx
    ON webhook_idempotency (inserted_at);
