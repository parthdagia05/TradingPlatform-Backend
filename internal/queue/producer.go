package queue

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Producer publishes events to the stream. The HTTP write path uses one of
// these — but it MUST NOT block the request: we use a 200ms timeout and log
// (don't fail) if Redis is degraded. A trade write succeeds in the DB even
// if the metrics event got dropped — those are eventually-consistent.
type Producer struct {
	rdb    *redis.Client
	stream string
}

// NewProducer binds a Producer to the given Redis client + stream name.
func NewProducer(rdb *redis.Client, stream string) *Producer {
	return &Producer{rdb: rdb, stream: stream}
}

// Publish appends one event to the stream. Returns the assigned stream ID
// (Redis-generated, monotonically increasing).
func (p *Producer) Publish(ctx context.Context, ev Event) (string, error) {
	id, err := p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: p.stream,
		// Redis-assigns the ID. "*" = "next monotonic value".
		ID: "*",
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
