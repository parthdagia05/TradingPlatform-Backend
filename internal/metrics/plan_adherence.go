package metrics

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PlanAdherence: rolling 10-trade average of planAdherence ratings per user.
//
// Run on every trade.closed event for the user. Reads the user's last 10
// closed trades (idx_trades_user_exit_closed makes this O(log n) seek + 10
// row reads), averages, and stores back to user_metrics.
func PlanAdherence(ctx context.Context, pool *pgxpool.Pool, repo *Repo, userID uuid.UUID) error {
	rows, err := pool.Query(ctx, `
        SELECT plan_adherence
          FROM trades
         WHERE user_id = $1
           AND status = 'closed'
           AND plan_adherence IS NOT NULL
         ORDER BY exit_at DESC
         LIMIT 10`, userID)
	if err != nil {
		return fmt.Errorf("query plan adherence trades: %w", err)
	}
	defer rows.Close()

	var sum, count int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return err
		}
		sum += v
		count++
	}
	if count == 0 {
		return nil // no data yet - nothing to update
	}
	avg := float64(sum) / float64(count)
	return repo.SetPlanAdherenceScore(ctx, userID, avg, count)
}
