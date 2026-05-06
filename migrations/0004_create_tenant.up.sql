-- 0004_create_tenant.up.sql
-- Tenant registry (SIN-62209). The `tenants` table is itself the source of
-- truth for which tenants exist; it is NOT tenant-scoped, so it has no
-- `tenant_id` column and no RLS.
--
-- Run as app_admin. Idempotent.
--
-- `app_runtime` only needs SELECT to resolve host -> tenant_id during
-- request routing; writes are gated behind app_admin / app_master_ops so
-- runtime cannot create or rename tenants.

BEGIN;

CREATE TABLE IF NOT EXISTS tenants (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name        text NOT NULL,
  host        text NOT NULL UNIQUE,
  created_at  timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE tenants OWNER TO app_admin;

REVOKE ALL ON tenants FROM PUBLIC;
GRANT SELECT ON tenants TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenants TO app_master_ops;

COMMIT;
