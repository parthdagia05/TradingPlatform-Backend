package trades

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned by repo lookups that find nothing.
var ErrNotFound = errors.New("trade not found")

// Repo encapsulates every SQL statement that touches the trades table.
// Keeping queries here (not in the handler) makes them reviewable in one place
// and lets us write integration tests against the repo without a chi router.
type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// Insert is the idempotent write path.
//
// Per spec: "POST /trades must be idempotent on tradeId. Duplicate submissions
// must return HTTP 200 with the existing record — not 500 or 409."
//
// We use ON CONFLICT (trade_id) DO NOTHING + RETURNING. If the row was newly
// inserted, RETURNING gives us the row. If it already existed, RETURNING is
// empty and we follow up with a SELECT — same return shape either way.
//
// `inserted` reports which path we took (true = new row, false = existed).
// The handler doesn't care; the test suite asserts on it for the idempotency
// proof.
func (r *Repo) Insert(ctx context.Context, in *TradeInput) (t *Trade, inserted bool, err error) {
	const insertSQL = `
        INSERT INTO trades (
            trade_id, user_id, session_id,
            asset, asset_class, direction,
            entry_price, exit_price, quantity,
            entry_at, exit_at, status,
            plan_adherence, emotional_state, entry_rationale,
            outcome, pnl
        ) VALUES (
            $1, $2, $3,
            $4, $5, $6,
            $7, $8, $9,
            $10, $11, $12,
            $13, $14, $15,
            $16, $17
        )
        ON CONFLICT (trade_id) DO NOTHING
        RETURNING ` + selectColumns

	out, pnl, outcome := derive(in)

	row := r.pool.QueryRow(ctx, insertSQL,
		in.TradeID, in.UserID, in.SessionID,
		in.Asset, string(in.AssetClass), string(in.Direction),
		in.EntryPrice, in.ExitPrice, in.Quantity,
		in.EntryAt, in.ExitAt, string(in.Status),
		in.PlanAdherence, nullableEnum(in.EmotionalState), in.EntryRationale,
		outcome, pnl,
	)
	t = &Trade{}
	if err := scan(row, t); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, false, fmt.Errorf("insert: %w", err)
		}
		// Row already existed — fetch it and return inserted=false.
		got, err := r.Get(ctx, in.TradeID)
		if err != nil {
			return nil, false, fmt.Errorf("idempotent fetch: %w", err)
		}
		return got, false, nil
	}
	out = t
	return out, true, nil
}

// Get fetches one trade by id. Returns ErrNotFound if absent.
func (r *Repo) Get(ctx context.Context, id uuid.UUID) (*Trade, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+selectColumns+` FROM trades WHERE trade_id = $1`, id)
	t := &Trade{}
	if err := scan(row, t); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get: %w", err)
	}
	return t, nil
}

// SetRevengeFlag updates the revenge_flag column from the async worker.
// We update directly here (vs returning the row) — the worker doesn't need it.
func (r *Repo) SetRevengeFlag(ctx context.Context, tradeID uuid.UUID, flag bool) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE trades SET revenge_flag = $1 WHERE trade_id = $2`, flag, tradeID)
	if err != nil {
		return fmt.Errorf("set revenge flag: %w", err)
	}
	return nil
}

// ── internal helpers ────────────────────────────────────────────────────────

// selectColumns is the canonical column list shared by Insert (RETURNING) and
// Get (SELECT). Keeping it as one constant guarantees the scan helper always
// matches the order.
const selectColumns = `
    trade_id, user_id, session_id,
    asset, asset_class, direction,
    entry_price, exit_price, quantity,
    entry_at, exit_at, status,
    plan_adherence, emotional_state, entry_rationale,
    outcome, pnl, revenge_flag,
    created_at, updated_at`

// scan reads one row in selectColumns order into t. Pointers for nullable cols.
func scan(row pgx.Row, t *Trade) error {
	var (
		assetClass, direction, status string
		emo, outcome                  *string
	)
	if err := row.Scan(
		&t.TradeID, &t.UserID, &t.SessionID,
		&t.Asset, &assetClass, &direction,
		&t.EntryPrice, &t.ExitPrice, &t.Quantity,
		&t.EntryAt, &t.ExitAt, &status,
		&t.PlanAdherence, &emo, &t.EntryRationale,
		&outcome, &t.PnL, &t.RevengeFlag,
		&t.CreatedAt, &t.UpdatedAt,
	); err != nil {
		return err
	}
	t.AssetClass = AssetClass(assetClass)
	t.Direction = Direction(direction)
	t.Status = Status(status)
	if emo != nil {
		es := EmotionalState(*emo)
		t.EmotionalState = &es
	}
	if outcome != nil {
		oc := Outcome(*outcome)
		t.Outcome = &oc
	}
	return nil
}

// derive computes server-side fields the client doesn't send: outcome and pnl.
// They're only meaningful for closed trades; null otherwise.
func derive(in *TradeInput) (placeholder *Trade, pnl any, outcome any) {
	if in.Status != StatusClosed || in.ExitPrice == nil {
		return nil, nil, nil
	}
	// PnL = (exit - entry) * quantity for long, inverted for short.
	delta := *in.ExitPrice - in.EntryPrice
	if in.Direction == DirShort {
		delta = -delta
	}
	p := delta * in.Quantity

	// Round to 8 decimal places to fit NUMERIC(18, 8). Float→numeric works
	// fine for the value range we care about (no extreme precision needed).
	p = round8(p)

	var oc Outcome = OutcomeWin
	if p < 0 {
		oc = OutcomeLoss
	}
	return nil, p, string(oc)
}

func round8(f float64) float64 {
	const scale = 1e8
	return float64(int64(f*scale+0.5*signf(f))) / scale
}
func signf(f float64) float64 {
	if f < 0 {
		return -1
	}
	return 1
}

// nullableEnum lets us pass a *EmotionalState through pgx as either text or NULL.
func nullableEnum(p *EmotionalState) any {
	if p == nil {
		return nil
	}
	return string(*p)
}

// Compile-time guard so callers know the signature.
var _ = time.Now
