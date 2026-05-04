-- 0006_create_sessions.up.sql
-- Per-tenant authenticated sessions (SIN-62209). Tenant-scoped via the
-- canonical four-policy template from docs/adr/0072-rls-policies.md.
--
-- Run as app_admin. Idempotent.
--
-- A session row exists for as long as the user holds a valid cookie. The
-- expires_at column is the authoritative TTL; expired rows are reaped by
-- a background job (out of scope for this PR). Master sessions live in a
-- separate table (future PR) so the standard RLS template applies cleanly
-- here without a NULL-tenant exception.

BEGIN;

CREATE TABLE IF NOT EXISTS sessions (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at  timestamptz NOT NULL,
  ip          inet,
  user_agent  text,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS sessions_tenant_id_expires_at_idx
  ON sessions (tenant_id, expires_at);

ALTER TABLE sessions OWNER TO app_admin;

ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions FORCE ROW LEVEL SECURITY;

REVOKE ALL ON sessions FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON sessions TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON sessions TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON sessions;
CREATE POLICY tenant_isolation_select ON sessions
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON sessions;
CREATE POLICY tenant_isolation_insert ON sessions
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON sessions;
CREATE POLICY tenant_isolation_update ON sessions
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON sessions;
CREATE POLICY tenant_isolation_delete ON sessions
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS sessions_master_ops_audit ON sessions;
CREATE TRIGGER sessions_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON sessions
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

COMMIT;
