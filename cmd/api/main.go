// Command api is the HTTP server.
//
// Boots in this order: load config, build logger, open Postgres + run
// migrations + (optionally) seed and backfill, open Redis + ensure consumer
// group, wire handlers and middleware, listen with graceful shutdown.
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
	"github.com/nevup/trade-journal/internal/metrics"
	mw "github.com/nevup/trade-journal/internal/middleware"
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
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := logger.New(cfg.LogLevel).With("component", "api")
	log.Info("starting api",
		"port", cfg.Port, "logLevel", cfg.LogLevel, "seedOnStart", cfg.SeedOnStart)

	// boot work runs on a separate, time-bounded context so a misconfigured
	// dependency can't keep the container hanging forever
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
		// only backfill metrics if the tables are empty; once the worker has
		// touched them we leave live state alone
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

	rdb, err := queue.NewClient(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	defer rdb.Close()

	if err := queue.EnsureGroup(bootCtx, rdb, cfg.StreamName, cfg.ConsumerGroup); err != nil {
		return fmt.Errorf("ensure group: %w", err)
	}
	producer := queue.NewProducer(rdb, cfg.StreamName)
	// /health uses its own consumer handle to read XPENDING
	healthConsumer := queue.NewConsumer(rdb, cfg.StreamName, cfg.ConsumerGroup,
		cfg.ConsumerName+"-health")

	tradeRepo := trades.NewRepo(pool)
	metricRepo := metrics.NewRepo(pool)
	verifier := auth.NewVerifier(cfg.JWTSecret)

	tradeHandler := trades.NewHandler(tradeRepo, producer)
	metricHandler := metrics.NewHandler(metricRepo)
	healthHandler := health.NewHandler(pool, healthConsumer)

	r := chi.NewRouter()
	// trace before logger so log lines carry traceId; recoverer last so it
	// catches panics from anything earlier in the chain
	r.Use(mw.Trace)
	r.Use(mw.Logger(log))
	r.Use(mw.Recoverer(log))

	healthHandler.Mount(r)

	// landing page for anyone who lands on / in a browser (e.g. the HF Space UI)
	r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(landingHTML))
	})

	r.Group(func(r chi.Router) {
		r.Use(mw.Authenticator(verifier))
		// /trades has no userId in the path; the handler does the body-level
		// tenancy check itself
		tradeHandler.Mount(r)

		// /users/{userId}/* gets the path-level tenancy guard
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

const landingHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>NevUp Trade Journal - Track 1</title>
<style>
  body { font: 14px/1.5 -apple-system, system-ui, sans-serif; max-width: 720px; margin: 4em auto; padding: 0 1em; color: #222; background: #fafafa; }
  h1 { font-size: 1.4em; margin-bottom: 0.2em; }
  .lede { color: #666; margin-top: 0; }
  code { background: #eef; padding: 1px 5px; border-radius: 3px; }
  table { border-collapse: collapse; margin: 1em 0; }
  th, td { text-align: left; padding: 6px 14px 6px 0; border-bottom: 1px solid #ddd; }
  th { font-weight: 600; }
  a { color: #0a58ca; }
  .ok { color: #0a7d2c; font-weight: 600; }
</style>
</head>
<body>
<h1>NevUp Trade Journal - Track 1 (System of Record)</h1>
<p class="lede">Live backend for the NevUp Hiring Hackathon 2026 submission.</p>
<p>Source &amp; docs: <a href="https://github.com/parthdagia05/TradingPlatform-Backend">github.com/parthdagia05/TradingPlatform-Backend</a></p>

<h2>Endpoints</h2>
<table>
  <tr><th>Method</th><th>Path</th><th>Auth</th></tr>
  <tr><td>GET</td><td><a href="/health"><code>/health</code></a></td><td>none</td></tr>
  <tr><td>POST</td><td><code>/trades</code></td><td>JWT</td></tr>
  <tr><td>GET</td><td><code>/trades/{tradeId}</code></td><td>JWT + tenancy</td></tr>
  <tr><td>GET</td><td><code>/users/{userId}/metrics?from=&amp;to=&amp;granularity=</code></td><td>JWT + tenancy</td></tr>
</table>

<p>Auth: HS256 JWT, secret published in the kickoff PDF. <code>jwt.sub</code> must match the <code>userId</code> in the path/body or the request is rejected with <span class="ok">403</span>, never 404.</p>
<p>Try <a href="/health"><code>/health</code></a> for a quick liveness check.</p>
</body>
</html>`
