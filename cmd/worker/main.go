// Command worker is the async metrics consumer.
// Pulls events off the Redis stream and runs the 5 metric calculators.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nevup/trade-journal/internal/config"
	"github.com/nevup/trade-journal/internal/db"
	"github.com/nevup/trade-journal/internal/logger"
	"github.com/nevup/trade-journal/internal/metrics"
	"github.com/nevup/trade-journal/internal/queue"
	"github.com/nevup/trade-journal/internal/trades"
	"github.com/nevup/trade-journal/internal/worker"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	log := logger.New(cfg.LogLevel).With("component", "worker")
	log.Info("starting worker",
		"stream", cfg.StreamName,
		"group", cfg.ConsumerGroup,
		"name", cfg.ConsumerName,
	)

	bootCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.NewPool(bootCtx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db pool: %w", err)
	}
	defer pool.Close()

	rdb, err := queue.NewClient(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	defer rdb.Close()

	if err := queue.EnsureGroup(bootCtx, rdb, cfg.StreamName, cfg.ConsumerGroup); err != nil {
		return fmt.Errorf("ensure group: %w", err)
	}
	consumer := queue.NewConsumer(rdb, cfg.StreamName, cfg.ConsumerGroup, cfg.ConsumerName)
	producer := queue.NewProducer(rdb, cfg.StreamName)

	tradeRepo := trades.NewRepo(pool)
	metricRepo := metrics.NewRepo(pool)

	w := worker.New(log, pool, consumer, producer, tradeRepo, metricRepo)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err = w.Run(ctx)
	log.Info("worker stopped", "reason", err)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
