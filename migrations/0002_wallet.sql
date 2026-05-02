-- SIN-62240 Wallet F30+F37 — token wallet aggregate + ledger.
--
-- Backward-compatible: introduces two new tables only; nothing existing is
-- altered or dropped. Rollback is `DROP TABLE token_ledger; DROP TABLE
-- token_wallets;` once Fase 1 (SIN-62193) is rolled back too.
--
-- Concurrency contract:
--   - token_wallets.balance_movement is the source of truth for "tokens left".
--   - Reserve takes SELECT ... FOR UPDATE on token_wallets and inserts a
--     row in token_ledger with status='pending' (NO change to balance_movement).
--   - Commit takes SELECT ... FOR UPDATE on the wallet row, flips the
--     pending entry to 'posted', and applies its signed amount.
--   - Cancel flips pending → cancelled (no balance change).
--   - We never DELETE from token_ledger; reconciliation creates entries
--     with source='reconciliation'.

BEGIN;

CREATE TABLE IF NOT EXISTS token_wallets (
    id                 UUID PRIMARY KEY,
    master_id          UUID NOT NULL,
    balance_movement   BIGINT NOT NULL DEFAULT 0,
    version            BIGINT NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_token_wallets_master_id
    ON token_wallets(master_id);

-- Ledger entries are append-only. status drives the lifecycle.
CREATE TABLE IF NOT EXISTS token_ledger (
    id            UUID PRIMARY KEY,
    wallet_id     UUID NOT NULL REFERENCES token_wallets(id) ON DELETE RESTRICT,
    status        TEXT NOT NULL CHECK (status IN ('pending','posted','cancelled')),
    kind          TEXT NOT NULL CHECK (kind IN ('debit','credit')),
    source        TEXT NOT NULL CHECK (source IN ('llm_call','reconciliation','grant','refund')),
    amount        BIGINT NOT NULL CHECK (amount > 0),
    reference     TEXT NOT NULL DEFAULT '',
    attempts      INTEGER NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    posted_at     TIMESTAMPTZ,
    cancelled_at  TIMESTAMPTZ
);

-- Drift detector and "list pending older than N" both scan by status.
CREATE INDEX IF NOT EXISTS idx_token_ledger_wallet_status
    ON token_ledger(wallet_id, status);

CREATE INDEX IF NOT EXISTS idx_token_ledger_status_created
    ON token_ledger(status, created_at);

COMMIT;
