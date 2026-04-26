// Command devstack runs the full Track 1 stack natively on the local machine
// — no Docker, no real Redis. Used when you can't (or don't want to) spin up
// docker-compose: ideal for low-disk machines, CI, or quick smoke tests.
//
// What it does:
//  1. Starts an in-process miniredis on REDIS_PORT (default 6380).
//  2. Wires the API + worker goroutines using the same code paths as
//     cmd/api and cmd/worker.
//  3. Loads config from env exactly like production — including DATABASE_URL.
//
// What it does NOT do:
//  - Manage Postgres. You bring that. See scripts/local-stack.sh for a
//    helper that sets up a user-owned cluster on /tmp.
//
// Production isolation: the Dockerfile only builds cmd/api and cmd/worker, so
// this binary never reaches the production image even without a build tag.
//
//	go run ./cmd/devstack
//
// Once running, the integration test suite works unchanged:
//
//	go test ./tests/...

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-chi/chi/v5"

	"github.com/nevup/trade-journal/internal/auth"
	"github.com/nevup/trade-journal/internal/config"
	"github.com/nevup/trade-journal/internal/db"
	"github.com/nevup/trade-journal/internal/health"
	"github.com/nevup/trade-journal/internal/logger"
	"github.com/nevup/trade-journal/internal/metrics"
	mw "github.com/nevup/trade-journal/internal/middleware"
	"github.com/nevup/trade-journal/internal/queue"
	"github.com/nevup/trade-journal/internal/trades"
	"github.com/nevup/trade-journal/internal/worker"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	// ── 1. Embedded Redis ───────────────────────────────────────────────────
	// miniredis listens on a random port by default; we pin it so the
	// REDIS_URL env points at the right place from the start.
	redisPort := envOr("REDIS_PORT", "6380")
	mr, err := miniredis.Run()
	if err != nil {
		return fmt.Errorf("miniredis: %w", err)
	}
	defer mr.Close()
	if err := relistenOn(mr, redisPort); err != nil {
		return fmt.Errorf("miniredis listen on :%s: %w", redisPort, err)
	}
	if os.Getenv("REDIS_URL") == "" {
		_ = os.Setenv("REDIS_URL", "redis://"+mr.Addr()+"/0")
	}

	// ── 2. Config + logger ──────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log := logger.New(cfg.LogLevel).With("component", "devstack")
	log.Info("devstack booting",
		"port", cfg.Port,
		"redis", os.Getenv("REDIS_URL"),
		"database", cfg.DatabaseURL,
	)

	// ── 3. Boot tasks: pool + migrations + seed + backfill ──────────────────
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

	// ── 4. Redis client + queue plumbing ────────────────────────────────────
	rdb, err := queue.NewClient(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("redis client: %w", err)
	}
	defer rdb.Close()

	if err := queue.EnsureGroup(bootCtx, rdb, cfg.StreamName, cfg.ConsumerGroup); err != nil {
		return fmt.Errorf("ensure group: %w", err)
	}
	producer := queue.NewProducer(rdb, cfg.StreamName)
	healthConsumer := queue.NewConsumer(rdb, cfg.StreamName, cfg.ConsumerGroup,
		cfg.ConsumerName+"-health")
	workerConsumer := queue.NewConsumer(rdb, cfg.StreamName, cfg.ConsumerGroup,
		cfg.ConsumerName)

	// ── 5. Repositories + handlers ──────────────────────────────────────────
	tradeRepo := trades.NewRepo(pool)
	metricRepo := metrics.NewRepo(pool)
	verifier := auth.NewVerifier(cfg.JWTSecret)

	tradeHandler := trades.NewHandler(tradeRepo, producer)
	metricHandler := metrics.NewHandler(metricRepo)
	healthHandler := health.NewHandler(pool, healthConsumer)

	r := chi.NewRouter()
	r.Use(mw.Trace)
	r.Use(mw.Logger(log))
	r.Use(mw.Recoverer(log))
	healthHandler.Mount(r)
	r.Group(func(r chi.Router) {
		r.Use(mw.Authenticator(verifier))
		tradeHandler.Mount(r)
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireUserMatch(func(req *http.Request) string {
				return chi.URLParam(req, "userId")
			}))
			metricHandler.Mount(r)
		})
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// ── 6. Start the worker as a goroutine (same code as cmd/worker) ────────
	w := worker.New(log, pool, workerConsumer, producer, tradeRepo, metricRepo)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		err := w.Run(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error("worker stopped with error", "err", err)
		}
	}()

	// ── 7. Run the HTTP server (blocking) with graceful shutdown ────────────
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

func envOr(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

// relistenOn forces miniredis to bind on a known port (default it picks random).
// We close its current listener and ask it to listen on the requested address.
func relistenOn(mr *miniredis.Miniredis, port string) error {
	if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("invalid port %q", port)
	}
	mr.Close()
	return mr.StartAddr("127.0.0.1:" + port)
}
