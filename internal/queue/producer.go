package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Producer publishes events to the stream. The HTTP write path uses one of
// these - but it MUST NOT block the request: we use a 200ms timeout and log
// (don't fail) if Redis is degraded. A trade write succeeds in the DB even
// if the metrics event got dropped - those are eventually-consistent.
type Producer struct {
	rdb    *redis.Client
	stream string
}

// NewProducer binds a Producer to the given Redis client + stream name.
func NewProducer(rdb *redis.Client, stream string) *Producer {
	return &Producer{rdb: rdb, stream: stream}
}

// Publish appends one event to the stream. Returns the assigned stream ID.
//
// We do one immediate retry on a transient error (e.g. a flaked TCP read)
// before giving up. The caller still wraps Publish in a tight timeout so
// retries can never drag a request past its p95 budget.
func (p *Producer) Publish(ctx context.Context, ev Event) (string, error) {
	id, err := p.publishOnce(ctx, ev)
	if err != nil && shouldRetry(ctx, err) {
		// brief backoff so we don't hammer a stressed redis
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		id, err = p.publishOnce(ctx, ev)
		if err != nil {
			return "", fmt.Errorf("xadd (after retry): %w", err)
		}
	}
	return id, err
}

func (p *Producer) publishOnce(ctx context.Context, ev Event) (string, error) {
	id, err := p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: p.stream,
		ID:     "*", // redis assigns the next monotonic id
		Values: map[string]any{
			"type":       string(ev.Type),
			"tradeId":    ev.TradeID,
			"userId":     ev.UserID,
			"sessionId":  ev.SessionID,
			"occurredAt": ev.OccurredAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		},
	}).Result()
	if err != nil {
		return "", fmt.Errorf("xadd: %w", err)
	}
	return id, nil
}

// shouldRetry reports whether an error is worth one more shot. Context errors
// (cancelled, deadline) are NOT retried - the caller has given up. Everything
// else (network blip, server-side wobble) we'll try once more.
func shouldRetry(ctx context.Context, err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	return true
}
