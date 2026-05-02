-- 0002_master_ops_audit.down.sql
-- Reverses 0002_master_ops_audit.up.sql. Run as app_admin. Idempotent.

BEGIN;

DROP FUNCTION IF EXISTS master_ops_audit_trigger();
DROP TABLE IF EXISTS master_ops_audit;

COMMIT;
