-- 0009_tenant_slug_redirect.down.sql — SIN-62244 (F46).
BEGIN;

DROP TRIGGER IF EXISTS tenant_slug_redirect_master_ops_audit
  ON tenant_slug_redirect;
DROP INDEX IF EXISTS tenant_slug_redirect_expires_at_idx;
DROP TABLE IF EXISTS tenant_slug_redirect;

COMMIT;
