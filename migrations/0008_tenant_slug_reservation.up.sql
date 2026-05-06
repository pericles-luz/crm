-- 0008_tenant_slug_reservation.up.sql — SIN-62244 (F46).
--
-- Slug reservation registry. When a tenant releases a slug (delete, slug
-- change, suspended >30d), an INSERT here locks the slug for 365 days so
-- it cannot be re-claimed by an attacker. The middleware
-- RequireSlugAvailable consults this table; the master override flow
-- soft-deletes by stamping `expires_at = now()`.
--
-- Like the `tenants` registry (0004) this table is itself the source of
-- truth for slug ownership; it is NOT tenant-scoped, so no `tenant_id`
-- column on the reservation row itself and no RLS. The
-- `released_by_tenant_id` column is a forensic pointer, not an authz
-- gate.
--
-- "INSERT-only" means runtime never UPDATEs or DELETEs reservation rows.
-- The master override is privileged (app_master_ops, MFA-gated, audited)
-- and stamps `expires_at = now()` to soft-delete; that UPDATE is allowed
-- to app_master_ops only.
--
-- Run as app_admin. Idempotent.

BEGIN;

CREATE TABLE IF NOT EXISTS tenant_slug_reservation (
  id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  slug                     text        NOT NULL,
  released_at              timestamptz NOT NULL,
  released_by_tenant_id    uuid,
  expires_at               timestamptz NOT NULL,
  created_at               timestamptz NOT NULL DEFAULT now()
);

-- Partial unique index: only ONE active reservation per slug at a time.
-- Once the master override stamps expires_at <= now() the slug is free
-- for the next legitimate tenant to claim, while history rows remain
-- queryable by ops.
CREATE UNIQUE INDEX IF NOT EXISTS tenant_slug_reservation_active_idx
  ON tenant_slug_reservation (slug)
  WHERE expires_at > now();

CREATE INDEX IF NOT EXISTS tenant_slug_reservation_released_at_idx
  ON tenant_slug_reservation (released_at);

ALTER TABLE tenant_slug_reservation OWNER TO app_admin;

REVOKE ALL ON tenant_slug_reservation FROM PUBLIC;
REVOKE ALL ON tenant_slug_reservation FROM app_runtime;
REVOKE ALL ON tenant_slug_reservation FROM app_master_ops;
-- Runtime path: SELECT (RequireSlugAvailable, redirect handler, master
-- console) and INSERT (release trigger called from tenant-delete /
-- slug-change / 30d-suspended jobs). UPDATE/DELETE are deliberately
-- withheld from app_runtime.
GRANT SELECT, INSERT ON tenant_slug_reservation TO app_runtime;
-- Master override soft-deletes by UPDATEing expires_at; needs UPDATE.
-- INSERT is also granted so support can pre-reserve sensitive slugs
-- (e.g. "admin", "support") prior to launch. DELETE remains revoked so
-- the audit trail cannot be erased — soft-delete is the only path.
GRANT SELECT, INSERT, UPDATE ON tenant_slug_reservation TO app_master_ops;

-- master_ops_audit_trigger captures every UPDATE/INSERT/DELETE done
-- under app_master_ops. The override endpoint runs inside
-- WithMasterOps so the trigger fires automatically.
DROP TRIGGER IF EXISTS tenant_slug_reservation_master_ops_audit
  ON tenant_slug_reservation;
CREATE TRIGGER tenant_slug_reservation_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON tenant_slug_reservation
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

COMMIT;
