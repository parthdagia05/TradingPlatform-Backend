// Package worker is the async pipeline orchestrator. It pulls events off the
// Redis stream and dispatches each to the right metric calculators. The five
// hackathon-required metrics live in internal/metrics - this package only wires.
//
// Delivery semantics: at-least-once. We only XACK a message after every
// calculator for it succeeded. If anything fails, the message stays in the
// consumer-group's PEL and a periodic recovery loop calls XCLAIM to retry it
// (potentially on a different consumer if the original is dead). After
// maxDeliveries attempts the message is dead-lettered (acked + logged) so a
// poison entry can't loop forever.
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

const (
	batchSize        = 32
	idleSleep        = 100 * time.Millisecond
	nonBlock         = -1 * time.Millisecond
	recoveryInterval = 30 * time.Second
	stuckIdleTime    = 30 * time.Second
	maxDeliveries    = 5
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

// Run is the consumer loop: poll, process, ack on success / leave in PEL on
// failure; every recoveryInterval we also XCLAIM messages that have gone idle
// past stuckIdleTime so a dead consumer's work gets picked up.
func (w *Worker) Run(ctx context.Context) error {
	w.log.Info("worker running",
		"batchSize", batchSize,
		"recoveryIntervalMs", recoveryInterval.Milliseconds(),
		"stuckIdleMs", stuckIdleTime.Milliseconds(),
		"maxDeliveries", maxDeliveries,
	)

	recovery := time.NewTicker(recoveryInterval)
	defer recovery.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-recovery.C:
			w.recoverStuck(ctx)
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

// recoverStuck claims any messages older than stuckIdleTime that nobody acked
// and re-runs them. Errors here are logged but never fatal - the next tick
// will try again.
func (w *Worker) recoverStuck(ctx context.Context) {
	msgs, err := w.consumer.ClaimStuck(ctx, stuckIdleTime, batchSize)
	if err != nil {
		w.log.Warn("xclaim recovery failed", "err", err)
		return
	}
	if len(msgs) == 0 {
		return
	}
	w.log.Info("recovered stuck messages", "count", len(msgs))
	for _, m := range msgs {
		w.handle(ctx, m)
	}
}

// handle dispatches one message. Acks on success, leaves it in PEL on
// transient failure (so XCLAIM picks it up later), dead-letters once we've
// crossed maxDeliveries.
func (w *Worker) handle(ctx context.Context, m queue.Message) {
	log := w.log.With(
		"messageId", m.ID,
		"type", m.Event.Type,
		"deliveries", m.DeliveryCount,
	)

	if m.DeliveryCount >= maxDeliveries {
		log.Error("dead-lettering message after max retries; acking to break the loop")
		if err := m.Ack(ctx); err != nil {
			log.Warn("dead-letter ack failed", "err", err)
		}
		return
	}

	failed, malformed := w.process(ctx, m, log)

	// malformed events are poison - they'll NEVER decode, so ack to drop them.
	// transient failures are NOT acked - the message stays in PEL for retry.
	if failed > 0 && !malformed {
		log.Warn("calculations failed; leaving in PEL for retry", "failures", failed)
		return
	}

	if err := m.Ack(ctx); err != nil {
		log.Warn("ack failed", "err", err)
	}
}

// process runs every calculator for the event type and returns:
//
//	failed    - how many calculators returned an error (0 = clean run)
//	malformed - true if the event itself is shape-broken (poison; ack-and-drop)
func (w *Worker) process(ctx context.Context, m queue.Message, log *slog.Logger) (failed int, malformed bool) {
	switch m.Event.Type {

	case queue.EventTradeOpened:
		tradeID, err1 := uuid.Parse(m.Event.TradeID)
		userID, err2 := uuid.Parse(m.Event.UserID)
		if err1 != nil || err2 != nil {
			log.Warn("malformed trade.opened event, dropping (not retryable)")
			return 0, true
		}
		if err := metrics.RevengeFlag(ctx, w.pool, w.metricRp,
			w.tradeRp.SetRevengeFlag, tradeID, userID); err != nil {
			log.Error("revenge_flag failed", "err", err, "tradeId", tradeID)
			failed++
		}
		if err := metrics.Overtrading(ctx, w.pool, w.metricRp,
			w.publishOvertradingEvent, userID, m.Event.OccurredAt); err != nil {
			log.Error("overtrading failed", "err", err, "userId", userID)
			failed++
		}

	case queue.EventTradeClosed:
		tradeID, err1 := uuid.Parse(m.Event.TradeID)
		userID, err2 := uuid.Parse(m.Event.UserID)
		sessionID, err3 := uuid.Parse(m.Event.SessionID)
		if err1 != nil || err2 != nil || err3 != nil {
			log.Warn("malformed trade.closed event, dropping (not retryable)")
			return 0, true
		}
		if err := metrics.PlanAdherence(ctx, w.pool, w.metricRp, userID); err != nil {
			log.Error("plan_adherence failed", "err", err, "userId", userID)
			failed++
		}
		if err := metrics.WinRateByEmotion(ctx, w.pool, w.metricRp, tradeID, userID); err != nil {
			log.Error("win_rate failed", "err", err, "tradeId", tradeID)
			failed++
		}
		if err := metrics.SessionTilt(ctx, w.metricRp, sessionID); err != nil {
			log.Error("session_tilt failed", "err", err, "sessionId", sessionID)
			failed++
		}

	case queue.EventOvertradingDetected:
		// emitted by ourselves on threshold crossing. Track 2's AI engine and
		// any other downstream consumers will pick this up. Track 1 has nothing
		// to do - the metric is already persisted in user_metrics +
		// overtrading_events. Acked as success.

	default:
		log.Warn("unknown event type, dropping (not retryable)")
		return 0, true
	}
	return failed, false
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
