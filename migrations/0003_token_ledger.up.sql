-- 0002_token_ledger.up.sql
-- Creates the append-only token_ledger table (SIN-62232).
--
-- This is the canonical example of the RLS template documented in
-- docs/adr/0072-rls-policies.md. Every future tenanted table MUST repeat
-- this structure (FORCE RLS + four policies + REVOKE for append-only).
--
-- Run as app_admin (BYPASSRLS=true is required to attach policies and to
-- own the table). Idempotent.

BEGIN;

CREATE TABLE IF NOT EXISTS token_ledger (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL,
  kind        text NOT NULL,
  amount      bigint NOT NULL,
  metadata    jsonb NOT NULL DEFAULT '{}'::jsonb,
  occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS token_ledger_tenant_id_occurred_at_idx
  ON token_ledger (tenant_id, occurred_at);

ALTER TABLE token_ledger OWNER TO app_admin;

ALTER TABLE token_ledger ENABLE ROW LEVEL SECURITY;
ALTER TABLE token_ledger FORCE ROW LEVEL SECURITY;

-- Per-table privileges. Append-only: app_runtime gets SELECT and INSERT only.
-- app_master_ops gets the same surface (cross-tenant via BYPASSRLS) but its
-- writes are audited by the master_ops_audit trigger below.
REVOKE ALL ON token_ledger FROM PUBLIC;
GRANT SELECT, INSERT ON token_ledger TO app_runtime;
GRANT SELECT, INSERT ON token_ledger TO app_master_ops;
REVOKE UPDATE, DELETE ON token_ledger FROM app_runtime;
REVOKE UPDATE, DELETE ON token_ledger FROM app_master_ops;

-- Tenant isolation policies. The canonical four-policy template from
-- docs/adr/0072-rls-policies.md. SELECT/INSERT only for token_ledger because
-- it is append-only; UPDATE/DELETE policies are intentionally absent so the
-- table-level REVOKE above is double-locked.
DROP POLICY IF EXISTS tenant_isolation_select ON token_ledger;
CREATE POLICY tenant_isolation_select ON token_ledger
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON token_ledger;
CREATE POLICY tenant_isolation_insert ON token_ledger
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- Master ops crosses tenants: it sees everything when BYPASSRLS=true. The
-- audit trigger below enforces accountability.
DROP TRIGGER IF EXISTS token_ledger_master_ops_audit ON token_ledger;
CREATE TRIGGER token_ledger_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON token_ledger
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

COMMIT;
