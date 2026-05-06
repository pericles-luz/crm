-- 0010_tenant_custom_domains.down.sql
-- Reverses 0010_tenant_custom_domains.up.sql. Run as app_admin. Idempotent.
--
-- Drop order: tenant_custom_domains first because it FKs into
-- dns_resolution_log; once that FK is gone the log table can drop too.

BEGIN;

DROP INDEX IF EXISTS idx_tenant_custom_domains_created_at;
DROP INDEX IF EXISTS idx_tenant_custom_domains_tenant_active;
DROP INDEX IF EXISTS uq_tenant_custom_domains_active_host;
DROP TABLE IF EXISTS tenant_custom_domains;

DROP INDEX IF EXISTS idx_dns_resolution_log_host_created_at;
DROP TABLE IF EXISTS dns_resolution_log;

COMMIT;
