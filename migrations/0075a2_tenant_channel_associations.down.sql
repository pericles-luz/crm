-- 0075a2_tenant_channel_associations.down.sql — SIN-62234 / ADR 0075 §6 rollback.
DROP INDEX IF EXISTS tenant_channel_associations_tenant_idx;
DROP TABLE IF EXISTS tenant_channel_associations;
