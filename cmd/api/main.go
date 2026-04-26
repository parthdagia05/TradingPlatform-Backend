// Command api is the HTTP server binary.
//
// Boot order:
//  1. Load config (fail fast on missing/invalid vars).
//  2. Build logger.
//  3. Open Postgres pool, run migrations, optionally seed the CSV.
//  4. Open Redis client, ensure stream consumer group exists.
//  5. Wire handlers (trades, metrics, health) and middleware
//     (trace → log → recoverer → auth → tenancy).
//  6. Listen on cfg.Port with graceful shutdown on SIGINT/SIGTERM.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nevup/trade-journal/internal/auth"
	"github.com/nevup/trade-journal/internal/config"
	"github.com/nevup/trade-journal/internal/db"
	"github.com/nevup/trade-journal/internal/health"
	"github.com/nevup/trade-journal/internal/logger"
	mw "github.com/nevup/trade-journal/internal/middleware"
	"github.com/nevup/trade-journal/internal/metrics"
	"github.com/nevup/trade-journal/internal/queue"
	"github.com/nevup/trade-journal/internal/trades"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// ── 1. Config ────────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// ── 2. Logger ────────────────────────────────────────────────────────────
	log := logger.New(cfg.LogLevel).With("component", "api")
	log.Info("starting api",
		"port", cfg.Port, "logLevel", cfg.LogLevel, "seedOnStart", cfg.SeedOnStart)

	// ── 3. Database ──────────────────────────────────────────────────────────
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer bootCancel()

	pool, err := db.NewPool(bootCtx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db pool: %w", err)
	}
	defer pool.Close()

	if err := db.RunMigrations(bootCtx, pool, cfg.MigrationsDir, log); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if cfg.SeedOnStart {
		if err := db.SeedFromCSV(bootCtx, pool, cfg.SeedFilePath, log); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
		// Backfill metric tables once, only if they're empty. The worker
		// owns metric updates for live trades — this just ensures seeded
		// users have queryable metrics on first boot.
		hasSnap, err := metrics.HasSnapshots(bootCtx, pool)
		if err != nil {
			return fmt.Errorf("check snapshots: %w", err)
		}
		if !hasSnap {
			if err := metrics.BackfillFromTrades(bootCtx, pool, log); err != nil {
				return fmt.Errorf("backfill: %w", err)
			}
		}
	}

	// ── 4. Redis (queue) ─────────────────────────────────────────────────────
	rdb, err := queue.NewClient(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	defer rdb.Close()

	if err := queue.EnsureGroup(bootCtx, rdb, cfg.StreamName, cfg.ConsumerGroup); err != nil {
		return fmt.Errorf("ensure group: %w", err)
	}
	producer := queue.NewProducer(rdb, cfg.StreamName)

	// /health uses a consumer-side handle to read pending count.
	healthConsumer := queue.NewConsumer(rdb, cfg.StreamName, cfg.ConsumerGroup,
		cfg.ConsumerName+"-health")

	// ── 5. Repositories + handlers ───────────────────────────────────────────
	tradeRepo := trades.NewRepo(pool)
	metricRepo := metrics.NewRepo(pool)

	tradeHandler := trades.NewHandler(tradeRepo, producer)
	metricHandler := metrics.NewHandler(metricRepo)
	healthHandler := health.NewHandler(pool, healthConsumer)

	verifier := auth.NewVerifier(cfg.JWTSecret)

	// ── 6. Router ────────────────────────────────────────────────────────────
	r := chi.NewRouter()

	// Cross-cutting middleware order matters:
	//   trace → logger → recoverer
	// (trace before logger so logs carry traceId; recoverer last so panics
	// inside earlier middleware also get caught).
	r.Use(mw.Trace)
	r.Use(mw.Logger(log))
	r.Use(mw.Recoverer(log))

	// /health is unauthenticated per spec.
	healthHandler.Mount(r)

	// Authenticated routes.
	r.Group(func(r chi.Router) {
		r.Use(mw.Authenticator(verifier))

		// /trades — auth-only; per-body cross-tenant check inside handler.
		tradeHandler.Mount(r)

		// /users/{userId}/* — auth + path-tenancy match.
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireUserMatch(func(req *http.Request) string {
				return chi.URLParam(req, "userId")
			}))
			metricHandler.Mount(r)
		})
	})

	// ── 7. HTTP server ───────────────────────────────────────────────────────
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received, draining...")
	case err := <-errCh:
		return fmt.Errorf("listen: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Info("bye")
	return nil
}
