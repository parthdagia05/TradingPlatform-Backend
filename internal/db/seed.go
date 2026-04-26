package db

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SeedFromCSV bulk-loads the hackathon seed file into the trades table.
//
// Idempotent on restart: if the trades table is non-empty, it does nothing.
// Otherwise it parses the file and uses Postgres COPY FROM (the fastest possible
// bulk-insert path) to load all rows in a single round-trip.
//
// CSV column order is fixed by the hackathon spec:
//
//	tradeId, userId, traderName, sessionId, asset, assetClass, direction,
//	entryPrice, exitPrice, quantity, entryAt, exitAt, status, outcome, pnl,
//	planAdherence, emotionalState, entryRationale, revengeFlag,
//	groundTruthPathologies
func SeedFromCSV(ctx context.Context, pool *pgxpool.Pool, path string, log *slog.Logger) error {
	// Skip if the table is already populated. The spec asks the seed data
	// to be present after `docker compose up`; it does NOT ask for it to be
	// re-loaded on every restart (that would clobber any new trades).
	var existing int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM trades`).Scan(&existing); err != nil {
		return fmt.Errorf("count trades: %w", err)
	}
	if existing > 0 {
		log.Info("seed skipped - trades table already populated", "rows", existing)
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open csv %q: %w", path, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.ReuseRecord = true
	r.FieldsPerRecord = 20 // hard-fail on malformed rows

	// First record is the header; discard but verify column count.
	if _, err := r.Read(); err != nil {
		return fmt.Errorf("read header: %w", err)
	}

	rows := make([][]any, 0, 400)
	for lineNo := 2; ; lineNo++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
		row, err := parseSeedRow(rec)
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
		// CopyFrom requires a stable backing slice per row. r.ReuseRecord = true
		// reuses the rec slice, but we already copied each element by value
		// inside parseSeedRow, so the rebound `row` is independent.
		rows = append(rows, row)
	}

	columns := []string{
		"trade_id", "user_id", "trader_name", "session_id",
		"asset", "asset_class", "direction",
		"entry_price", "exit_price", "quantity",
		"entry_at", "exit_at", "status",
		"outcome", "pnl", "plan_adherence",
		"emotional_state", "entry_rationale",
		"revenge_flag", "ground_truth_pathologies",
	}

	inserted, err := pool.CopyFrom(ctx,
		pgx.Identifier{"trades"},
		columns,
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return fmt.Errorf("copy from: %w", err)
	}
	log.Info("seed loaded", "rows", inserted, "source", path)
	return nil
}

// parseSeedRow turns one CSV record into a 20-element []any whose order
// matches the `columns` slice in SeedFromCSV. nil entries become NULL in COPY.
func parseSeedRow(rec []string) ([]any, error) {
	if len(rec) != 20 {
		return nil, fmt.Errorf("expected 20 fields, got %d", len(rec))
	}

	entryAt, err := parseTime(rec[10])
	if err != nil {
		return nil, fmt.Errorf("entryAt: %w", err)
	}
	exitAt, err := parseTimeNullable(rec[11])
	if err != nil {
		return nil, fmt.Errorf("exitAt: %w", err)
	}

	entryPrice, err := strconv.ParseFloat(rec[7], 64)
	if err != nil {
		return nil, fmt.Errorf("entryPrice %q: %w", rec[7], err)
	}
	quantity, err := strconv.ParseFloat(rec[9], 64)
	if err != nil {
		return nil, fmt.Errorf("quantity %q: %w", rec[9], err)
	}
	exitPrice, err := parseFloatNullable(rec[8])
	if err != nil {
		return nil, fmt.Errorf("exitPrice: %w", err)
	}
	pnl, err := parseFloatNullable(rec[14])
	if err != nil {
		return nil, fmt.Errorf("pnl: %w", err)
	}

	planAdh, err := parseIntNullable(rec[15])
	if err != nil {
		return nil, fmt.Errorf("planAdherence: %w", err)
	}

	revenge, err := strconv.ParseBool(rec[18])
	if err != nil {
		return nil, fmt.Errorf("revengeFlag %q: %w", rec[18], err)
	}

	return []any{
		rec[0],                  // trade_id (string  UUID by pgx)
		rec[1],                  // user_id
		emptyToNil(rec[2]),      // trader_name
		rec[3],                  // session_id
		rec[4],                  // asset
		rec[5],                  // asset_class (string  enum)
		rec[6],                  // direction
		entryPrice,              // entry_price
		exitPrice,               // exit_price (nullable)
		quantity,                // quantity
		entryAt,                 // entry_at
		exitAt,                  // exit_at (nullable)
		rec[12],                 // status
		emptyToNil(rec[13]),     // outcome (nullable)
		pnl,                     // pnl (nullable)
		planAdh,                 // plan_adherence (nullable)
		emptyToNil(rec[16]),     // emotional_state (nullable)
		emptyToNil(rec[17]),     // entry_rationale (nullable)
		revenge,                 // revenge_flag
		emptyToNil(rec[19]),     // ground_truth_pathologies (nullable)
	}, nil
}

// emptyToNil maps ""  nil (= SQL NULL). Anything else passes through unchanged.
func emptyToNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// parseTime parses an ISO-8601 / RFC3339 timestamp. We use RFC3339Nano so
// values with fractional seconds (".000Z" in our CSV) are accepted.
func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

// parseTimeNullable returns nil for empty input, otherwise parses the time.
func parseTimeNullable(s string) (any, error) {
	if s == "" {
		return nil, nil
	}
	t, err := parseTime(s)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func parseFloatNullable(s string) (any, error) {
	if s == "" {
		return nil, nil
	}
	return strconv.ParseFloat(s, 64)
}

func parseIntNullable(s string) (any, error) {
	if s == "" {
		return nil, nil
	}
	return strconv.Atoi(s)
}
