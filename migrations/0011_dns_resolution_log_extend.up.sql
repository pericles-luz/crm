-- 0011_dns_resolution_log_extend.up.sql
-- SIN-62333 F52 — Wire dns_resolution_log writer to validation pipeline.
--
-- Extends the dns_resolution_log table created in 0010 so every
-- Validate / ValidateHostOnly call from internal/customdomain/validation
-- can persist a row keyed on (tenant_id, host, decision, reason).
-- ADR 0079 §1 already required pinning the resolved IP on success;
-- F52 closes the OWASP A09 gap by also persisting blocked/error rows
-- so post-incident forensics on a blocked SSRF attempt is possible.
--
-- Schema delta:
--   - tenant_id UUID NULL  — populated by the writer when the request
--     carries a tenant context. NULL for anonymous/internal calls.
--   - decision TEXT NOT NULL — controlled vocabulary: allow|block|error.
--   - reason TEXT NOT NULL — controlled vocabulary mirroring the audit
--     event vocabulary in internal/customdomain/validation/ports.go.
--   - phase TEXT NOT NULL — validate (full check) or host_only (Enroll
--     pre-flight). Lets the IR query distinguish the two callsites.
--   - pinned_ip is now NULLABLE. Blocked-SSRF rows MUST persist NULL
--     because the validator INTENTIONALLY discards the attacker-chosen
--     address; it must never round-trip into our log pipeline.
--
-- Defaults are dropped after the column add so every future INSERT must
-- be explicit. Keeping a default silently masks a writer bug — a row
-- would land with decision='allow' for what was actually a block.
--
-- Rollback: 0011_dns_resolution_log_extend.down.sql drops the new
-- columns and restores NOT NULL on pinned_ip. The table itself stays
-- (it is referenced by tenant_custom_domains.dns_resolution_log_id).
-- Append-only: only INSERTs land here, so dropping or truncating
-- discards forensics rows but loses no business data.

BEGIN;

ALTER TABLE dns_resolution_log
    ADD COLUMN IF NOT EXISTS tenant_id UUID NULL,
    ADD COLUMN IF NOT EXISTS decision TEXT NOT NULL DEFAULT 'allow',
    ADD COLUMN IF NOT EXISTS reason TEXT NOT NULL DEFAULT 'ok',
    ADD COLUMN IF NOT EXISTS phase TEXT NOT NULL DEFAULT 'validate';

ALTER TABLE dns_resolution_log
    ALTER COLUMN decision DROP DEFAULT,
    ALTER COLUMN reason DROP DEFAULT,
    ALTER COLUMN phase DROP DEFAULT;

ALTER TABLE dns_resolution_log
    ALTER COLUMN pinned_ip DROP NOT NULL;

ALTER TABLE dns_resolution_log
    DROP CONSTRAINT IF EXISTS dns_resolution_log_decision_chk;
ALTER TABLE dns_resolution_log
    ADD CONSTRAINT dns_resolution_log_decision_chk
    CHECK (decision IN ('allow', 'block', 'error'));

ALTER TABLE dns_resolution_log
    DROP CONSTRAINT IF EXISTS dns_resolution_log_phase_chk;
ALTER TABLE dns_resolution_log
    ADD CONSTRAINT dns_resolution_log_phase_chk
    CHECK (phase IN ('validate', 'host_only'));

-- Block rows MUST NOT carry a pinned IP — the attacker-chosen address
-- must never reach the log pipeline. Enforce structurally.
ALTER TABLE dns_resolution_log
    DROP CONSTRAINT IF EXISTS dns_resolution_log_block_no_ip_chk;
ALTER TABLE dns_resolution_log
    ADD CONSTRAINT dns_resolution_log_block_no_ip_chk
    CHECK (decision <> 'block' OR pinned_ip IS NULL);

CREATE INDEX IF NOT EXISTS idx_dns_resolution_log_tenant_created
    ON dns_resolution_log(tenant_id, created_at DESC)
    WHERE tenant_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_dns_resolution_log_decision_reason
    ON dns_resolution_log(decision, reason, created_at DESC);

GRANT SELECT, INSERT ON dns_resolution_log TO app_runtime;
GRANT SELECT, INSERT ON dns_resolution_log TO app_master_ops;
REVOKE UPDATE, DELETE ON dns_resolution_log FROM app_runtime;

COMMIT;
