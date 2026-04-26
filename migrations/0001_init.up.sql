-- 0001_init.up.sql
-- Forward migration: create the trades table, supporting types, and indexes.
-- Naming convention: NNNN_<description>.up.sql / .down.sql
-- The numeric prefix is the migration version. Lexical sort = apply order.

-- Enum types
-- Using Postgres native ENUMs (vs TEXT + CHECK) for two reasons:
--   1. Storage: enums are stored as 4-byte integers, not the string itself.
--   2. Type safety: an invalid enum value fails at INSERT time with a clean error.
-- Trade-off: adding a value later requires `ALTER TYPE ... ADD VALUE`. Acceptable.

CREATE TYPE asset_class      AS ENUM ('equity', 'crypto', 'forex');
CREATE TYPE trade_direction  AS ENUM ('long', 'short');
CREATE TYPE trade_status     AS ENUM ('open', 'closed', 'cancelled');
CREATE TYPE trade_outcome    AS ENUM ('win', 'loss');
CREATE TYPE emotional_state  AS ENUM ('calm', 'anxious', 'greedy', 'fearful', 'neutral');

-- trades - the system of record
-- One row per trade. Idempotent on trade_id (the natural key from the client).
CREATE TABLE trades (
    trade_id                    UUID            PRIMARY KEY,
    user_id                     UUID            NOT NULL,
    trader_name                 TEXT,                                      -- denormalized helper from seed; nullable
    session_id                  UUID            NOT NULL,
    asset                       TEXT            NOT NULL,
    asset_class                 asset_class     NOT NULL,
    direction                   trade_direction NOT NULL,
    entry_price                 NUMERIC(18, 8)  NOT NULL CHECK (entry_price > 0),
    exit_price                  NUMERIC(18, 8),
    quantity                    NUMERIC(18, 8)  NOT NULL CHECK (quantity > 0),
    entry_at                    TIMESTAMPTZ     NOT NULL,
    exit_at                     TIMESTAMPTZ,
    status                      trade_status    NOT NULL,
    outcome                     trade_outcome,
    pnl                         NUMERIC(18, 8),
    plan_adherence              SMALLINT        CHECK (plan_adherence BETWEEN 1 AND 5),
    emotional_state             emotional_state,
    entry_rationale             TEXT            CHECK (entry_rationale IS NULL OR length(entry_rationale) <= 500),
    revenge_flag                BOOLEAN         NOT NULL DEFAULT FALSE,
    ground_truth_pathologies    TEXT,                                      -- AI training label (Track 2 use), kept for analytics
    created_at                  TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    updated_at                  TIMESTAMPTZ     NOT NULL DEFAULT NOW(),

    -- A closed trade MUST have an exit timestamp + price.
    -- This rule belongs in the DB, not just the app, so a buggy worker can never
    -- leave the table in an inconsistent state.
    CONSTRAINT closed_has_exit CHECK (
        status <> 'closed' OR (exit_at IS NOT NULL AND exit_price IS NOT NULL)
    )
);

-- Indexes - each one justified by a specific query in DECISIONS.md

-- Most user queries filter by user_id and a time range over entry_at.
-- DESC order matches the typical "show me recent trades" access pattern.
CREATE INDEX idx_trades_user_entry
    ON trades (user_id, entry_at DESC);

-- The async worker's "find this user's last 10 closed trades" and "did a loss
-- close in the last 90 seconds?" queries use this. Partial index = smaller +
-- faster because we exclude open/cancelled rows that are never relevant here.
CREATE INDEX idx_trades_user_exit_closed
    ON trades (user_id, exit_at DESC)
    WHERE status = 'closed';

-- Session Tilt Index walks all trades in the current session, in entry order.
CREATE INDEX idx_trades_session
    ON trades (session_id, entry_at);

-- Win Rate by Emotional State groups by (user_id, emotional_state, outcome).
-- Partial index restricted to closed trades because outcome is null otherwise.
CREATE INDEX idx_trades_user_emotion_outcome
    ON trades (user_id, emotional_state, outcome)
    WHERE status = 'closed';

-- updated_at trigger
-- Why a DB trigger instead of doing this in Go?
--   1. Centralizes the rule - every UPDATE bumps it, regardless of caller.
--   2. The async worker also updates rows (revenge_flag, etc.) and must not
--      forget to set updated_at - the DB enforces it for us.
CREATE OR REPLACE FUNCTION trg_trades_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trades_set_updated_at
BEFORE UPDATE ON trades
FOR EACH ROW
EXECUTE FUNCTION trg_trades_set_updated_at();
