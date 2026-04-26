package metrics

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WinRateByEmotion: per-user running win/loss count grouped by emotionalState.
//
// Run on every trade.closed event. We read the just-closed trade's
// (emotional_state, outcome) and bump the appropriate counter via the repo.
// This is O(1) per event vs. recomputing the full aggregate from scratch.
func WinRateByEmotion(ctx context.Context, pool *pgxpool.Pool, repo *Repo,
	tradeID, userID uuid.UUID,
) error {
	var (
		emo     *string
		outcome *string
	)
	err := pool.QueryRow(ctx, `
        SELECT emotional_state, outcome
          FROM trades WHERE trade_id = $1`, tradeID).Scan(&emo, &outcome)
	if err != nil {
		return fmt.Errorf("load closed trade: %w", err)
	}
	if emo == nil || outcome == nil {
		return nil
	}
	won := *outcome == "win"
	return repo.UpsertEmotionStat(ctx, userID, *emo, won)
}
