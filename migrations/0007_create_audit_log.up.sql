-- 0007_create_audit_log.up.sql
-- Per-tenant business-event audit log (SIN-62209). Append-only; mirrors
-- the token_ledger template from docs/adr/0072-rls-policies.md.
--
-- Run as app_admin. Idempotent.
--
-- Distinct from `master_ops_audit` (which captures cross-tenant statements
-- executed under app_master_ops): `audit_log` is the tenant's own ledger of
-- business events (login, role grant, record export, etc.). It is
-- tenant-scoped, append-only at the policy AND privilege level, and
-- reachable from app_runtime so middleware/handlers can write entries
-- without escalating role.

BEGIN;

CREATE TABLE IF NOT EXISTS audit_log (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  actor_user_id   uuid REFERENCES users(id) ON DELETE SET NULL,
  event           text NOT NULL,
  target          jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS audit_log_tenant_id_created_at_idx
  ON audit_log (tenant_id, created_at);

ALTER TABLE audit_log OWNER TO app_admin;

ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE ROW LEVEL SECURITY;

REVOKE ALL ON audit_log FROM PUBLIC;
GRANT SELECT, INSERT ON audit_log TO app_runtime;
GRANT SELECT, INSERT ON audit_log TO app_master_ops;
REVOKE UPDATE, DELETE ON audit_log FROM app_runtime;
REVOKE UPDATE, DELETE ON audit_log FROM app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON audit_log;
CREATE POLICY tenant_isolation_select ON audit_log
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON audit_log;
CREATE POLICY tenant_isolation_insert ON audit_log
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS audit_log_master_ops_audit ON audit_log;
CREATE TRIGGER audit_log_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON audit_log
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

COMMIT;
