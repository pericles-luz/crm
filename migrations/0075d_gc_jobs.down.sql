-- 0075d_gc_jobs.down.sql — SIN-62234 / ADR 0075 §6 rollback.
DROP FUNCTION IF EXISTS webhook_create_raw_event_partition(date);
DROP FUNCTION IF EXISTS webhook_drop_raw_event_partition(text);
DROP FUNCTION IF EXISTS webhook_gc_idempotency(interval);
