-- 0011_dns_resolution_log_extend.down.sql
-- Reverses 0011_dns_resolution_log_extend.up.sql. Run as app_admin.
-- Idempotent.
--
-- The dns_resolution_log table itself stays — it is created by 0010 and
-- referenced by tenant_custom_domains.dns_resolution_log_id. Dropping
-- the new columns restores the F45 schema. No business data is lost
-- because dns_resolution_log is append-only audit; rolling back drops
-- forensics rows but no domain state.

BEGIN;

REVOKE INSERT ON dns_resolution_log FROM app_runtime;
REVOKE SELECT ON dns_resolution_log FROM app_runtime;
REVOKE INSERT ON dns_resolution_log FROM app_master_ops;
REVOKE SELECT ON dns_resolution_log FROM app_master_ops;

DROP INDEX IF EXISTS idx_dns_resolution_log_decision_reason;
DROP INDEX IF EXISTS idx_dns_resolution_log_tenant_created;

ALTER TABLE dns_resolution_log
    DROP CONSTRAINT IF EXISTS dns_resolution_log_block_no_ip_chk;
ALTER TABLE dns_resolution_log
    DROP CONSTRAINT IF EXISTS dns_resolution_log_phase_chk;
ALTER TABLE dns_resolution_log
    DROP CONSTRAINT IF EXISTS dns_resolution_log_decision_chk;

ALTER TABLE dns_resolution_log
    DROP COLUMN IF EXISTS phase,
    DROP COLUMN IF EXISTS reason,
    DROP COLUMN IF EXISTS decision,
    DROP COLUMN IF EXISTS tenant_id;

-- Restore the original 0010 NOT NULL on pinned_ip. This requires
-- truncating any rows with NULL pinned_ip first; on rollback this
-- discards block/error rows the writer added. Acceptable because the
-- table is append-only audit and we are rolling back the schema that
-- introduced those row shapes.
DELETE FROM dns_resolution_log WHERE pinned_ip IS NULL;
ALTER TABLE dns_resolution_log
    ALTER COLUMN pinned_ip SET NOT NULL;

COMMIT;
