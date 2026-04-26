// Package db owns the Postgres connection pool, migration runner, and the
// app's typed query helpers (added in later phases).
//
// We use pgx/v5's native pool (pgxpool), not database/sql. pgx is faster,
// supports COPY FROM (used by the seeder for bulk insert), and has better
// handling of NUMERIC, UUID, TIMESTAMPTZ - the exact types our schema uses.
package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool builds a connection pool from a DSN like
// "postgres://user:pass@host:5432/db?sslmode=disable".
//
// We tune two pool settings explicitly:
//   - MaxConns:    upper bound on concurrent connections to Postgres.
//   - MinConns:    keep some idle connections warm so a request never pays
//                  the TCP+TLS handshake cost on a cold pool.
//
// The 10s connect timeout means we fail fast on a misconfigured DATABASE_URL
// instead of hanging forever - important for the docker-compose health flow.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	// Pool sizing rationale:
	//   - 200 concurrent close events/sec target × ~20ms per write = ~4 in flight.
	//   - We give 25 max conns to absorb spikes and leave headroom for the worker.
	//   - 5 min conns keep a warm pool so p95 isn't dragged by a cold start.
	cfg.MaxConns = 25
	cfg.MinConns = 5
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute

	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(connectCtx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(connectCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// RunMigrations applies every NNNN_*.up.sql in dir that hasn't been applied yet,
// each one inside a transaction, recording the version in schema_migrations.
//
// This is a deliberately tiny migration runner - ~50 lines vs pulling in
// golang-migrate as a dep. For a single-digit number of migrations it's plenty.
// If migrations grow past ~20, switch to golang-migrate.
func RunMigrations(ctx context.Context, pool *pgxpool.Pool, dir string, log *slog.Logger) error {
	// 1. Ensure the bookkeeping table exists. Idempotent.
	const ddl = `
        CREATE TABLE IF NOT EXISTS schema_migrations (
            version    TEXT PRIMARY KEY,
            applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
        );
    `
	if _, err := pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	// 2. List all .up.sql files, sorted lexically. The NNNN_ prefix makes
	// lexical sort = chronological order. (0001 < 0002 < 0010 < 0100.)
	pattern := filepath.Join(dir, "*.up.sql")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob %s: %w", pattern, err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no migrations found at %s", pattern)
	}
	sort.Strings(files)

	// 3. Apply each file in its own transaction. If one fails, the partial
	// state rolls back automatically - no half-applied schema.
	for _, file := range files {
		version := strings.TrimSuffix(filepath.Base(file), ".up.sql")

		var alreadyApplied bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`,
			version,
		).Scan(&alreadyApplied)
		if err != nil {
			return fmt.Errorf("check %s: %w", version, err)
		}
		if alreadyApplied {
			log.Debug("migration already applied", "version", version)
			continue
		}

		body, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("read %s: %w", file, err)
		}

		if err := applyOne(ctx, pool, version, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", version, err)
		}
		log.Info("migration applied", "version", version)
	}
	return nil
}

// applyOne runs a single migration's SQL and records it, atomically.
func applyOne(ctx context.Context, pool *pgxpool.Pool, version, sqlText string) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	// On any error path, ensure rollback. Commit before this defer makes
	// rollback a no-op (Postgres knows the tx is already finished).
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, sqlText); err != nil {
		return fmt.Errorf("exec sql: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1)`,
		version,
	); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// IsUniqueViolation reports whether err is a Postgres "duplicate key" error.
// We expose this so the trades handler can detect "already inserted" without
// importing pgx directly. (Idempotency on POST /trades will use it.)
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505" // SQLSTATE for unique_violation
	}
	return false
}
