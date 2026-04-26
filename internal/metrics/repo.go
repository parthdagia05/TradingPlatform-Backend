package metrics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repo is the metrics package's gateway to the DB. Read AND write helpers,
// one place to look. Worker calls write helpers; HTTP handler calls read ones.
type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// ─── Reads ────────────────────────────────────────────────────────────────

// LoadSnapshot fetches the latest user_metrics row + emotion stats. Returns
// zero values if no rows exist yet (cold start, no events processed).
func (r *Repo) LoadSnapshot(ctx context.Context, userID uuid.UUID) (UserMetrics, error) {
	out := UserMetrics{
		UserID:           userID,
		WinRateByEmotion: map[string]EmotionStats{},
	}

	// user_metrics row.
	var planScore *float64
	err := r.pool.QueryRow(ctx, `
        SELECT plan_adherence_score, revenge_trades_count, overtrading_events_count
          FROM user_metrics WHERE user_id = $1`, userID).
		Scan(&planScore, &out.RevengeTrades, &out.OvertradingEvents)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, fmt.Errorf("user_metrics: %w", err)
	}
	out.PlanAdherenceScore = planScore

	// emotion stats.
	rows, err := r.pool.Query(ctx, `
        SELECT emotional_state, wins, losses
          FROM user_emotion_metrics WHERE user_id = $1`, userID)
	if err != nil {
		return out, fmt.Errorf("user_emotion_metrics: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			emo            string
			wins, losses   int
		)
		if err := rows.Scan(&emo, &wins, &losses); err != nil {
			return out, err
		}
		var rate float64
		if total := wins + losses; total > 0 {
			rate = float64(wins) / float64(total)
		}
		out.WinRateByEmotion[emo] = EmotionStats{Wins: wins, Losses: losses, WinRate: rate}
	}

	// Latest tilt index across user's recent sessions (avg).
	var tilt *float64
	if err := r.pool.QueryRow(ctx, `
        SELECT AVG(tilt_index)::FLOAT8
          FROM session_metrics WHERE user_id = $1`, userID).Scan(&tilt); err != nil &&
		!errors.Is(err, pgx.ErrNoRows) {
		return out, fmt.Errorf("session_metrics avg: %w", err)
	}
	out.SessionTiltIndex = tilt

	return out, nil
}

// LoadBuckets returns a timeseries of buckets between [from, to] for the user.
// We compute on-the-fly from the trades table — index idx_trades_user_entry
// makes this fast (sub-100ms even for years of data with proper indexing).
func (r *Repo) LoadBuckets(ctx context.Context,
	userID uuid.UUID, from, to time.Time, gran Granularity,
) ([]Bucket, error) {
	bucketExpr, err := bucketExpression(gran)
	if err != nil {
		return nil, err
	}

	q := fmt.Sprintf(`
        SELECT %s AS bucket_at,
               COUNT(*) AS trade_count,
               COUNT(*) FILTER (WHERE outcome = 'win')::FLOAT8 / NULLIF(COUNT(*) FILTER (WHERE outcome IN ('win','loss')), 0) AS win_rate,
               COALESCE(SUM(pnl), 0)::FLOAT8 AS pnl,
               COALESCE(AVG(plan_adherence)::FLOAT8, 0) AS avg_plan
          FROM trades
         WHERE user_id  = $1
           AND entry_at >= $2
           AND entry_at <  $3
         GROUP BY bucket_at
         ORDER BY bucket_at`, bucketExpr)

	rows, err := r.pool.Query(ctx, q, userID, from, to)
	if err != nil {
		return nil, fmt.Errorf("query buckets: %w", err)
	}
	defer rows.Close()

	var out []Bucket
	for rows.Next() {
		var b Bucket
		var winRate *float64
		if err := rows.Scan(&b.BucketAt, &b.TradeCount, &winRate, &b.PnL, &b.AvgPlanAdherence); err != nil {
			return nil, err
		}
		if winRate != nil {
			b.WinRate = *winRate
		}
		out = append(out, b)
	}
	return out, nil
}

// bucketExpression returns the SQL date_trunc / case expression for one row's
// bucket. We use date_trunc which is index-friendly.
func bucketExpression(g Granularity) (string, error) {
	switch g {
	case GranHourly:
		return "date_trunc('hour', entry_at)", nil
	case GranDaily:
		return "date_trunc('day', entry_at)", nil
	case GranRolling30:
		// rolling30d: one bucket per day, but the windowed statistics are still
		// per-day. Frontends compute the rolling window on the response.
		return "date_trunc('day', entry_at)", nil
	default:
		return "", fmt.Errorf("invalid granularity %q", g)
	}
}

// ─── Writes (called by the worker) ─────────────────────────────────────────

// SetPlanAdherenceScore upserts the rolling-window score on user_metrics.
func (r *Repo) SetPlanAdherenceScore(ctx context.Context, userID uuid.UUID, score float64, window int) error {
	_, err := r.pool.Exec(ctx, `
        INSERT INTO user_metrics (user_id, plan_adherence_score, plan_adherence_window)
        VALUES ($1, $2, $3)
        ON CONFLICT (user_id) DO UPDATE
        SET plan_adherence_score  = EXCLUDED.plan_adherence_score,
            plan_adherence_window = EXCLUDED.plan_adherence_window,
            updated_at            = NOW()`,
		userID, score, window)
	return err
}

// IncrementRevengeCount bumps the lifetime counter by 1.
func (r *Repo) IncrementRevengeCount(ctx context.Context, userID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
        INSERT INTO user_metrics (user_id, revenge_trades_count)
        VALUES ($1, 1)
        ON CONFLICT (user_id) DO UPDATE
        SET revenge_trades_count = user_metrics.revenge_trades_count + 1,
            updated_at           = NOW()`,
		userID)
	return err
}

// IncrementOvertradingCount bumps the lifetime counter by 1.
func (r *Repo) IncrementOvertradingCount(ctx context.Context, userID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
        INSERT INTO user_metrics (user_id, overtrading_events_count)
        VALUES ($1, 1)
        ON CONFLICT (user_id) DO UPDATE
        SET overtrading_events_count = user_metrics.overtrading_events_count + 1,
            updated_at               = NOW()`,
		userID)
	return err
}

// AddOvertradingEvent records the detected window so the API can list/count.
func (r *Repo) AddOvertradingEvent(ctx context.Context,
	userID uuid.UUID, windowStart, windowEnd time.Time, count int) error {
	_, err := r.pool.Exec(ctx, `
        INSERT INTO overtrading_events (event_id, user_id, window_start, window_end, trade_count)
        VALUES ($1, $2, $3, $4, $5)`,
		uuid.New(), userID, windowStart, windowEnd, count)
	return err
}

// UpsertEmotionStat increments wins or losses for (user, emotion).
func (r *Repo) UpsertEmotionStat(ctx context.Context, userID uuid.UUID, emo string, won bool) error {
	col := "losses"
	if won {
		col = "wins"
	}
	q := fmt.Sprintf(`
        INSERT INTO user_emotion_metrics (user_id, emotional_state, %s)
        VALUES ($1, $2, 1)
        ON CONFLICT (user_id, emotional_state) DO UPDATE
        SET %s        = user_emotion_metrics.%s + 1,
            updated_at = NOW()`, col, col, col)
	_, err := r.pool.Exec(ctx, q, userID, emo)
	return err
}

// UpsertSessionTilt recomputes and stores tilt for the session by reading
// the trades table directly. We group by (session_id, user_id) — every trade
// in a session shares the same user_id (data invariant), so this is exact
// and avoids needing a MIN aggregate over UUID (which Postgres lacks).
func (r *Repo) UpsertSessionTilt(ctx context.Context, sessionID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
        WITH ordered AS (
            SELECT session_id, user_id,
                   LAG(outcome) OVER (PARTITION BY session_id ORDER BY entry_at) AS prev_outcome
              FROM trades
             WHERE session_id = $1
               AND status = 'closed'
        )
        INSERT INTO session_metrics
            (session_id, user_id, total_trades, loss_following_trades, tilt_index, updated_at)
        SELECT session_id,
               user_id,
               COUNT(*)::int,
               COUNT(*) FILTER (WHERE prev_outcome = 'loss')::int,
               COALESCE(
                   COUNT(*) FILTER (WHERE prev_outcome = 'loss')::numeric
                   / NULLIF(COUNT(*), 0)::numeric,
                   0
               )::numeric(5,4),
               NOW()
          FROM ordered
         GROUP BY session_id, user_id
        ON CONFLICT (session_id) DO UPDATE
        SET total_trades          = EXCLUDED.total_trades,
            loss_following_trades = EXCLUDED.loss_following_trades,
            tilt_index            = EXCLUDED.tilt_index,
            updated_at            = NOW()`,
		sessionID)
	return err
}
