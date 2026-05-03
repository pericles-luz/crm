-- SIN-62240 Wallet F30+F37 — token wallet aggregate + ledger.
--
-- Backward-compatible: introduces two new tables only; nothing existing is
-- altered or dropped. Rollback is `DROP TABLE token_ledger; DROP TABLE
-- token_wallets;` once Fase 1 (SIN-62193) is rolled back too.
--
-- Concurrency contract (F30 atomic reserve):
--   - token_wallets.balance_movement is the source of truth for "tokens
--     left right now" — a debit reduces it the moment Reserve runs,
--     before the LLM call, so two concurrent Reserves cannot both see
--     enough balance and oversubscribe.
--   - Reserve takes SELECT ... FOR UPDATE on the wallet row, validates
--     balance_movement >= amount, then debits balance_movement (adds the
--     entry's signed amount) and inserts a token_ledger row with
--     status='pending'. Reserve owns the balance change.
--   - Commit flips the pending entry to 'posted' (status-only) and bumps
--     wallet.version. NO change to balance_movement — Reserve already
--     applied it. This is what makes the F37 retry loop safe.
--   - Cancel flips pending → cancelled and restores the reserved tokens
--     to balance_movement (reverses the Reserve mutation).
--   - We never DELETE from token_ledger; reconciliation creates entries
--     with source='reconciliation'.
--   - Defense in depth: the balance_movement >= 0 CHECK is the DB-level
--     invariant that backs up the application's atomic-reserve guard.
--
-- Drift invariant (used by the nightly reconciliator):
--   For any wallet in steady state (no pending entries):
--     sum_posted_signed + (initial_balance - balance_movement) == 0
--   initial_balance is the genesis-grant snapshot captured at wallet
--   creation; it never changes. The reconciliator divides the residual
--   by initial_balance to compute drift_pct, so the column MUST be
--   populated at insert time, not left at the default 0.

BEGIN;

CREATE TABLE IF NOT EXISTS token_wallets (
    id                 UUID PRIMARY KEY,
    master_id          UUID NOT NULL,
    -- initial_balance is the genesis grant — the value of balance_movement
    -- at wallet creation. It is the denominator of the nightly drift
    -- formula and MUST NOT change over the wallet's lifetime.
    initial_balance    BIGINT NOT NULL DEFAULT 0 CHECK (initial_balance >= 0),
    -- Defense in depth: balance_movement >= 0 is the DB-level invariant
    -- backing the application's atomic-reserve guard. If a code path
    -- ever tries to drain the wallet past zero, this CHECK is the
    -- safety net.
    balance_movement   BIGINT NOT NULL DEFAULT 0 CHECK (balance_movement >= 0),
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
