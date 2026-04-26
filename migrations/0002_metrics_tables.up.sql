-- 0002_metrics_tables.up.sql
-- Storage for the 5 behavioural metrics computed by the async worker.
-- These tables are populated EXCLUSIVELY by the worker (see internal/metrics/*).
-- The read API reads from here for fast, indexed lookups instead of recomputing.

-- ─────────────────────────────────────────────────────────────────────────────
-- user_metrics — one row per user, aggregate / snapshot state
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE user_metrics (
    user_id                     UUID            PRIMARY KEY,
    -- Plan Adherence Score: rolling 10-trade average. Recomputed on each close.
    plan_adherence_score        NUMERIC(5, 4),
    plan_adherence_window       INTEGER         NOT NULL DEFAULT 0,
    -- Lifetime counters used by the metrics endpoint's totals.
    revenge_trades_count        INTEGER         NOT NULL DEFAULT 0,
    overtrading_events_count    INTEGER         NOT NULL DEFAULT 0,
    updated_at                  TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

-- ─────────────────────────────────────────────────────────────────────────────
-- user_emotion_metrics — wins/losses bucketed by (user, emotional_state)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE user_emotion_metrics (
    user_id          UUID                NOT NULL,
    emotional_state  emotional_state     NOT NULL,
    wins             INTEGER             NOT NULL DEFAULT 0,
    losses           INTEGER             NOT NULL DEFAULT 0,
    updated_at       TIMESTAMPTZ         NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, emotional_state)
);

-- ─────────────────────────────────────────────────────────────────────────────
-- session_metrics — tilt index per session
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE session_metrics (
    session_id              UUID            PRIMARY KEY,
    user_id                 UUID            NOT NULL,
    total_trades            INTEGER         NOT NULL DEFAULT 0,
    loss_following_trades   INTEGER         NOT NULL DEFAULT 0,
    tilt_index              NUMERIC(5, 4)   NOT NULL DEFAULT 0,
    last_outcome            trade_outcome,
    last_exit_at            TIMESTAMPTZ,
    updated_at              TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_session_metrics_user ON session_metrics (user_id, updated_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- overtrading_events — append-only log of detected overtrading windows
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE overtrading_events (
    event_id        UUID            PRIMARY KEY,
    user_id         UUID            NOT NULL,
    window_start    TIMESTAMPTZ     NOT NULL,
    window_end      TIMESTAMPTZ     NOT NULL,
    trade_count     INTEGER         NOT NULL,
    detected_at     TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_overtrading_user_time ON overtrading_events (user_id, detected_at DESC);
