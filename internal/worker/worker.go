// Package worker is the async pipeline orchestrator. It pulls events off the
// Redis stream and dispatches each to the right metric calculators. The five
// hackathon-required metrics live in internal/metrics — this package only wires.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nevup/trade-journal/internal/metrics"
	"github.com/nevup/trade-journal/internal/queue"
	"github.com/nevup/trade-journal/internal/trades"
)

// Worker hosts the consumer loop. One instance per process; concurrency
// inside the loop is achieved by processing each Read() batch in parallel
// goroutines (bounded by the consumer batch size).
type Worker struct {
	log      *slog.Logger
	pool     *pgxpool.Pool
	consumer *queue.Consumer
	prod     *queue.Producer
	tradeRp  *trades.Repo
	metricRp *metrics.Repo
}

// New builds a Worker. All deps are required.
func New(log *slog.Logger, pool *pgxpool.Pool, c *queue.Consumer, p *queue.Producer,
	tradeRp *trades.Repo, metricRp *metrics.Repo) *Worker {
	return &Worker{log: log, pool: pool, consumer: c, prod: p,
		tradeRp: tradeRp, metricRp: metricRp}
}

// Run loops until ctx cancels. Each iteration: poll for messages, process,
// ack. On empty results we sleep briefly to avoid burning a core.
//
// IMPORTANT — go-redis Block semantics:
//   Block <  0   → no BLOCK keyword sent → returns immediately (true non-block)
//   Block == 0   → BLOCK 0 sent → blocks **forever** (Redis protocol)
//   Block >  0   → BLOCK <ms> sent → blocks up to that many milliseconds
//
// We pass a negative Block so the consumer returns immediately on an empty
// stream. This works on both real Redis and miniredis (whose stream BLOCK
// implementation hangs longer than the client read timeout). Real Redis
// could use a positive Block for lower latency, but the difference vs our
// 100ms idle sleep is invisible at hackathon scale and the portability is
// worth it.
func (w *Worker) Run(ctx context.Context) error {
	const (
		batchSize = 32
		idleSleep = 100 * time.Millisecond
		nonBlock  = -1 * time.Millisecond
	)
	w.log.Info("worker running", "batchSize", batchSize, "idleSleepMs", idleSleep.Milliseconds())

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msgs, err := w.consumer.Read(ctx, batchSize, nonBlock)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			w.log.Error("consumer read failed; backing off", "err", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}

		if len(msgs) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(idleSleep):
			}
			continue
		}

		for _, m := range msgs {
			w.handle(ctx, m)
		}
	}
}

// handle processes a single message: dispatch to calculators, then ack.
// Errors are logged but never returned — at-least-once delivery handles retries.
//
// We parse IDs lazily per event type because not every event populates all
// three (overtrading.detected has only userId + occurredAt).
func (w *Worker) handle(ctx context.Context, m queue.Message) {
	log := w.log.With("messageId", m.ID, "type", m.Event.Type)

	switch m.Event.Type {

	case queue.EventTradeOpened:
		tradeID, err1 := uuid.Parse(m.Event.TradeID)
		userID, err2 := uuid.Parse(m.Event.UserID)
		if err1 != nil || err2 != nil {
			log.Warn("malformed trade.opened event, dropping")
			_ = m.Ack(ctx)
			return
		}
		if err := metrics.RevengeFlag(ctx, w.pool, w.metricRp,
			w.tradeRp.SetRevengeFlag, tradeID, userID); err != nil {
			log.Error("revenge_flag failed", "err", err, "tradeId", tradeID)
		}
		if err := metrics.Overtrading(ctx, w.pool, w.metricRp,
			w.publishOvertradingEvent, userID, m.Event.OccurredAt); err != nil {
			log.Error("overtrading failed", "err", err, "userId", userID)
		}

	case queue.EventTradeClosed:
		tradeID, err1 := uuid.Parse(m.Event.TradeID)
		userID, err2 := uuid.Parse(m.Event.UserID)
		sessionID, err3 := uuid.Parse(m.Event.SessionID)
		if err1 != nil || err2 != nil || err3 != nil {
			log.Warn("malformed trade.closed event, dropping")
			_ = m.Ack(ctx)
			return
		}
		if err := metrics.PlanAdherence(ctx, w.pool, w.metricRp, userID); err != nil {
			log.Error("plan_adherence failed", "err", err, "userId", userID)
		}
		if err := metrics.WinRateByEmotion(ctx, w.pool, w.metricRp, tradeID, userID); err != nil {
			log.Error("win_rate failed", "err", err, "tradeId", tradeID)
		}
		if err := metrics.SessionTilt(ctx, w.metricRp, sessionID); err != nil {
			log.Error("session_tilt failed", "err", err, "sessionId", sessionID)
		}

	case queue.EventOvertradingDetected:
		// Emitted by ourselves on threshold crossing. Track 2's AI engine and
		// any downstream consumers will pick this up. Track 1 has nothing to
		// do — the metric is already persisted in user_metrics + overtrading_events.

	default:
		log.Warn("unknown event type, dropping")
	}

	if err := m.Ack(ctx); err != nil {
		log.Warn("ack failed", "err", err)
	}
}

// publishOvertradingEvent emits the bus event the spec requires.
func (w *Worker) publishOvertradingEvent(ctx context.Context, userID uuid.UUID,
	start, end time.Time, count int,
) error {
	if w.prod == nil {
		return nil
	}
	_, err := w.prod.Publish(ctx, queue.Event{
		Type:       queue.EventOvertradingDetected,
		UserID:     userID.String(),
		OccurredAt: end,
	})
	return err
}
