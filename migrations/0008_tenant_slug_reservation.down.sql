-- 0008_tenant_slug_reservation.down.sql — SIN-62244 (F46).
BEGIN;

DROP TRIGGER IF EXISTS tenant_slug_reservation_master_ops_audit
  ON tenant_slug_reservation;
DROP INDEX IF EXISTS tenant_slug_reservation_released_at_idx;
DROP INDEX IF EXISTS tenant_slug_reservation_active_idx;
DROP TABLE IF EXISTS tenant_slug_reservation;

COMMIT;
