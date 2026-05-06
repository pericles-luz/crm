-- 0075a_webhook_tokens.down.sql — SIN-62234 / ADR 0075 §6 rollback.
DROP INDEX IF EXISTS webhook_tokens_tenant_idx;
DROP INDEX IF EXISTS webhook_tokens_active_idx;
DROP TABLE IF EXISTS webhook_tokens;
