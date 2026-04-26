package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Overtrading: > 10 trades opened in any 30-minute sliding window per user.
//
// Run on every trade.opened event. The check is anchored at the current
// trade's entry_at — count opens in the [entry_at - 30m, entry_at] window.
// If count > 10, record an overtrading_event and bump the counter.
//
// The same emit-once-per-incident behaviour matters: we don't want to emit
// a second event for trade #12 if we already emitted one for trade #11
// in the same window. We dedupe by checking for any overtrading_event whose
// window covers entry_at.
func Overtrading(ctx context.Context, pool *pgxpool.Pool, repo *Repo,
	publisher func(context.Context, uuid.UUID, time.Time, time.Time, int) error,
	userID uuid.UUID, entryAt time.Time,
) error {
	windowStart := entryAt.Add(-30 * time.Minute)

	var count int
	err := pool.QueryRow(ctx, `
        SELECT COUNT(*)
          FROM trades
         WHERE user_id  = $1
           AND entry_at >= $2
           AND entry_at <= $3`, userID, windowStart, entryAt).Scan(&count)
	if err != nil {
		return fmt.Errorf("count opens in window: %w", err)
	}
	if count <= 10 {
		return nil
	}

	// Dedupe: if we already detected an overtrading window covering entry_at
	// in the last 30 minutes, skip.
	var existing int
	err = pool.QueryRow(ctx, `
        SELECT COUNT(*)
          FROM overtrading_events
         WHERE user_id     = $1
           AND window_end >= $2
           AND window_end <= $3`,
		userID, windowStart, entryAt).Scan(&existing)
	if err != nil {
		return fmt.Errorf("dedupe check: %w", err)
	}
	if existing > 0 {
		return nil
	}

	if err := repo.AddOvertradingEvent(ctx, userID, windowStart, entryAt, count); err != nil {
		return err
	}
	if err := repo.IncrementOvertradingCount(ctx, userID); err != nil {
		return err
	}
	if publisher != nil {
		// Best-effort emit on the bus (spec: "emit an overtrading event onto
		// the event bus"). Don't fail the metric write if the bus is degraded.
		_ = publisher(ctx, userID, windowStart, entryAt, count)
	}
	return nil
}
