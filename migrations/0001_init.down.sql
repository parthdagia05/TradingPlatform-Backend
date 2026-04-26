-- 0001_init.down.sql
-- Reverse migration: undo everything 0001_init.up.sql created.
-- Drop in reverse dependency order: trigger - function - table - enums.

DROP TRIGGER IF EXISTS trades_set_updated_at ON trades;
DROP FUNCTION IF EXISTS trg_trades_set_updated_at();

DROP TABLE IF EXISTS trades;

DROP TYPE IF EXISTS emotional_state;
DROP TYPE IF EXISTS trade_outcome;
DROP TYPE IF EXISTS trade_status;
DROP TYPE IF EXISTS trade_direction;
DROP TYPE IF EXISTS asset_class;
