package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RevengeFlag: a trade is "revenge" if it OPENS within 90 seconds of a
// losing CLOSE for the same user AND its emotionalState is anxious or fearful.
//
// Run on every trade.opened event. We:
//   1. Look up the just-opened trade's entry_at + emotional_state.
//   2. Find the most recent losing close for the user before entry_at.
//   3. If (entry_at - exit_at) <= 90s AND emo ∈ {anxious, fearful}, flag.
//
// Updates trades.revenge_flag and bumps the user's lifetime counter.
func RevengeFlag(ctx context.Context, pool *pgxpool.Pool, repo *Repo,
	tradeRepoSetFlag func(context.Context, uuid.UUID, bool) error,
	tradeID, userID uuid.UUID,
) error {
	var (
		entryAt time.Time
		emo     *string
	)
	err := pool.QueryRow(ctx, `
        SELECT entry_at, emotional_state
          FROM trades WHERE trade_id = $1`, tradeID).Scan(&entryAt, &emo)
	if err != nil {
		return fmt.Errorf("load opened trade: %w", err)
	}
	if emo == nil || (*emo != "anxious" && *emo != "fearful") {
		return nil // emotion doesn't qualify
	}

	var lastLossExit *time.Time
	err = pool.QueryRow(ctx, `
        SELECT exit_at
          FROM trades
         WHERE user_id = $1
           AND status = 'closed'
           AND outcome = 'loss'
           AND exit_at IS NOT NULL
           AND exit_at <= $2
         ORDER BY exit_at DESC
         LIMIT 1`, userID, entryAt).Scan(&lastLossExit)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil
		}
		return fmt.Errorf("query last loss: %w", err)
	}
	if lastLossExit == nil {
		return nil
	}
	if entryAt.Sub(*lastLossExit) > 90*time.Second {
		return nil
	}

	if err := tradeRepoSetFlag(ctx, tradeID, true); err != nil {
		return fmt.Errorf("set revenge flag: %w", err)
	}
	return repo.IncrementRevengeCount(ctx, userID)
}
