-- 0075b_webhook_idempotency.down.sql — SIN-62234 / ADR 0075 §6 rollback.
DROP INDEX IF EXISTS webhook_idempotency_gc_idx;
DROP TABLE IF EXISTS webhook_idempotency;
