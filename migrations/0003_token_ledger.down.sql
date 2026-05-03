-- 0002_token_ledger.down.sql
-- Reverses 0002_token_ledger.up.sql. Run as app_admin. Idempotent.

BEGIN;

DROP TRIGGER IF EXISTS token_ledger_master_ops_audit ON token_ledger;
DROP POLICY IF EXISTS tenant_isolation_select ON token_ledger;
DROP POLICY IF EXISTS tenant_isolation_insert ON token_ledger;
DROP INDEX IF EXISTS token_ledger_tenant_id_occurred_at_idx;
DROP TABLE IF EXISTS token_ledger;

COMMIT;
