// Command bootcheck is a developer-only utility (NOT shipped to production —
// the Dockerfile only builds cmd/api and cmd/worker).
// It exercises migrations + seed + backfill against a local Postgres and
// dumps EXPLAIN ANALYZE for the read-API's hottest queries.
//
//	go run ./cmd/bootcheck
//
// Configuration via env (sane defaults for the local socket Postgres):
//   DATABASE_URL    postgres://nevup@/nevup?host=/tmp&port=5433&sslmode=disable
//   MIGRATIONS_DIR  ./migrations
//   SEED_FILE_PATH  ./nevup_seed_dataset.csv

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nevup/trade-journal/internal/db"
	"github.com/nevup/trade-journal/internal/metrics"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := envOr("DATABASE_URL",
		"postgres://nevup@/nevup?host=/tmp&port=5433&sslmode=disable")
	migDir := envOr("MIGRATIONS_DIR", "./migrations")
	csvPath := envOr("SEED_FILE_PATH", "./nevup_seed_dataset.csv")

	log := slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		return fmt.Errorf("pool: %w", err)
	}
	defer pool.Close()

	t0 := time.Now()
	if err := db.RunMigrations(ctx, pool, migDir, log); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	log.Info("migrations ok", "elapsedMs", time.Since(t0).Milliseconds())

	t0 = time.Now()
	if err := db.SeedFromCSV(ctx, pool, csvPath, log); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	log.Info("seed ok", "elapsedMs", time.Since(t0).Milliseconds())

	t0 = time.Now()
	if err := metrics.BackfillFromTrades(ctx, pool, log); err != nil {
		return fmt.Errorf("backfill: %w", err)
	}
	log.Info("backfill ok", "elapsedMs", time.Since(t0).Milliseconds())

	dump(ctx, pool, "user_metrics",
		`SELECT user_id, plan_adherence_score, plan_adherence_window,
                revenge_trades_count, overtrading_events_count
           FROM user_metrics ORDER BY user_id`)
	dump(ctx, pool, "user_emotion_metrics (head 10)",
		`SELECT user_id, emotional_state, wins, losses
           FROM user_emotion_metrics ORDER BY user_id, emotional_state LIMIT 10`)
	dump(ctx, pool, "session_metrics (top 5 by trade count)",
		`SELECT session_id, total_trades, loss_following_trades, tilt_index
           FROM session_metrics ORDER BY total_trades DESC LIMIT 5`)
	dump(ctx, pool, "overtrading_events per user",
		`SELECT user_id, COUNT(*) AS events
           FROM overtrading_events GROUP BY user_id ORDER BY events DESC`)
	dump(ctx, pool, "revenge_flag per user",
		`SELECT user_id, COUNT(*) FILTER (WHERE revenge_flag) AS revenge_n
           FROM trades GROUP BY user_id ORDER BY revenge_n DESC`)

	uid := "f412f236-4edc-47a2-8f54-8763a6ed2ce8" // Alex Mercer
	explain(ctx, pool, "GET /users/:id/metrics  — bucketed timeseries query", `
        SELECT date_trunc('day', entry_at) AS bucket_at,
               COUNT(*) AS trade_count,
               COUNT(*) FILTER (WHERE outcome = 'win')::FLOAT8
                 / NULLIF(COUNT(*) FILTER (WHERE outcome IN ('win','loss')), 0) AS win_rate,
               COALESCE(SUM(pnl), 0)::FLOAT8 AS pnl,
               COALESCE(AVG(plan_adherence)::FLOAT8, 0) AS avg_plan
          FROM trades
         WHERE user_id  = $1
           AND entry_at >= '2025-01-01T00:00:00Z'
           AND entry_at <  '2025-04-01T00:00:00Z'
         GROUP BY bucket_at
         ORDER BY bucket_at`, uid)

	explain(ctx, pool, "Worker — plan adherence last 10 closed trades", `
        SELECT plan_adherence FROM trades
         WHERE user_id = $1 AND status = 'closed' AND plan_adherence IS NOT NULL
         ORDER BY exit_at DESC LIMIT 10`, uid)

	explain(ctx, pool, "Worker — revenge flag: last losing close", `
        SELECT exit_at FROM trades
         WHERE user_id = $1 AND status = 'closed' AND outcome = 'loss'
           AND exit_at IS NOT NULL AND exit_at <= '2025-02-15T12:00:00Z'
         ORDER BY exit_at DESC LIMIT 1`, uid)

	return nil
}

func envOr(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func dump(ctx context.Context, pool *pgxpool.Pool, label, q string) {
	fmt.Printf("\n=== %s ===\n", label)
	rows, err := pool.Query(ctx, q)
	if err != nil {
		fmt.Println("ERROR:", err)
		return
	}
	defer rows.Close()
	descs := rows.FieldDescriptions()
	for rows.Next() {
		vals, _ := rows.Values()
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = fmt.Sprintf("%s=%v", descs[i].Name, v)
		}
		fmt.Println("  " + strings.Join(parts, "  "))
	}
}

func explain(ctx context.Context, pool *pgxpool.Pool, label, q string, args ...any) {
	fmt.Printf("\n=== EXPLAIN: %s ===\n", label)
	rows, err := pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) "+q, args...)
	if err != nil {
		fmt.Println("ERROR:", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		_ = rows.Scan(&line)
		fmt.Println(" ", line)
	}
}
