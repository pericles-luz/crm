-- 0005_create_users.up.sql
-- Users table (SIN-62209). Tenant-scoped via the canonical four-policy
-- template from docs/adr/0072-rls-policies.md.
--
-- Run as app_admin. Idempotent.
--
-- Master users are an explicit exception to the "tenant_id NOT NULL"
-- invariant: the row carries `is_master = true` and `tenant_id IS NULL`,
-- and is therefore invisible to `app_runtime` (RLS policies compare
-- `tenant_id = current_setting('app.tenant_id')::uuid` which can never
-- match NULL). Master users are read/written exclusively through
-- `app_master_ops` (BYPASSRLS, audited via 0002_master_ops_audit).
-- The CHECK constraint enforces the (master <=> NULL tenant_id) shape so
-- a regular tenant user cannot accidentally land with NULL tenant_id and
-- become invisible.

BEGIN;

CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE IF NOT EXISTS users (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid REFERENCES tenants(id) ON DELETE CASCADE,
  email           citext NOT NULL,
  password_hash   text NOT NULL,
  role            text NOT NULL DEFAULT 'agent',
  is_master       boolean NOT NULL DEFAULT false,
  created_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT users_master_xor_tenant CHECK (
    (is_master = true  AND tenant_id IS NULL) OR
    (is_master = false AND tenant_id IS NOT NULL)
  )
);

CREATE UNIQUE INDEX IF NOT EXISTS users_tenant_email_idx
  ON users (tenant_id, email)
  WHERE tenant_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS users_master_email_idx
  ON users (email)
  WHERE is_master = true;

ALTER TABLE users OWNER TO app_admin;

ALTER TABLE users ENABLE ROW LEVEL SECURITY;
ALTER TABLE users FORCE ROW LEVEL SECURITY;

REVOKE ALL ON users FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON users TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON users TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON users;
CREATE POLICY tenant_isolation_select ON users
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON users;
CREATE POLICY tenant_isolation_insert ON users
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON users;
CREATE POLICY tenant_isolation_update ON users
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON users;
CREATE POLICY tenant_isolation_delete ON users
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS users_master_ops_audit ON users;
CREATE TRIGGER users_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON users
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

COMMIT;
