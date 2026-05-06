-- 0075d_gc_jobs.up.sql — SIN-62234 / ADR 0075 §3 (GC of webhook_idempotency)
-- and §5 (raw_event partition rotation).
--
-- This migration documents and provides idempotent, repeatable SQL that
-- the application's nightly cron (internal/worker/webhook_partition.go)
-- invokes. We deliberately implement GC as plain SQL functions instead
-- of pg_cron / pg_partman extensions:
--
--   * pg_cron requires superuser permissions and an extension enabled by
--     the DB operator — heavier ops dependency than the rest of the stack.
--   * pg_partman ditto, plus it manages parent/child partitions which we
--     can do in two short SQL statements.
--   * App-side cron (a Go worker) keeps the choice in the same idiom as
--     the reconciler (D7) and is auditable in code review.
--
-- The functions are SECURITY INVOKER so the cron account's grants apply.

CREATE OR REPLACE FUNCTION webhook_gc_idempotency(retention interval)
RETURNS bigint
LANGUAGE sql
SECURITY INVOKER
AS $$
    WITH deleted AS (
        DELETE FROM webhook_idempotency
         WHERE inserted_at < now() - retention
        RETURNING 1
    )
    SELECT COUNT(*) FROM deleted;
$$;

-- Helper used by the partition-rotation worker to drop a single
-- raw_event partition by inclusive-end date. The worker computes the
-- name from the date and checks pg_class first (NOTICE on no-op).
CREATE OR REPLACE FUNCTION webhook_drop_raw_event_partition(part_name text)
RETURNS void
LANGUAGE plpgsql
SECURITY INVOKER
AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = part_name) THEN
        EXECUTE format('DROP TABLE IF EXISTS %I', part_name);
    END IF;
END;
$$;

-- Helper that creates the partition for a given UTC date if missing.
-- Idempotent; safe to call from the daily cron at any time.
CREATE OR REPLACE FUNCTION webhook_create_raw_event_partition(part_date date)
RETURNS void
LANGUAGE plpgsql
SECURITY INVOKER
AS $$
DECLARE
    part_name text := format('raw_event_%s', to_char(part_date, 'YYYYMMDD'));
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF raw_event FOR VALUES FROM (%L) TO (%L)',
        part_name,
        part_date,
        part_date + INTERVAL '1 day'
    );
END;
$$;
