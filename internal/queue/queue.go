// Package queue is our async pipeline boundary. We use Redis Streams as the
// transport — the spec accepts Redis Streams as a "real" message queue
// (Kafka / RabbitMQ / equivalent). Streams give us: durable append-only log,
// consumer-group fan-out, at-least-once delivery, and millisecond latency.
//
// Two kinds of events flow through this stream:
//
//   - "trade.closed"     — the worker computes plan-adherence, revenge-flag,
//                          session-tilt, win-rate.
//   - "trade.opened"     — the worker checks the 30-min sliding window for
//                          overtrading and emits "overtrading.detected" if so.
//   - "overtrading.detected" — same stream; recorded in the DB by the worker.
//
// One stream, multiple event types, distinguished by the "type" field. This
// is simpler than multiple streams and lets us preserve event ordering per user.
package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// EventType is the discriminator on each stream entry.
type EventType string

const (
	EventTradeOpened         EventType = "trade.opened"
	EventTradeClosed         EventType = "trade.closed"
	EventOvertradingDetected EventType = "overtrading.detected"
)

// Event is what producers publish and consumers receive. Keep it small —
// Redis Streams is not for big payloads. The full trade lives in Postgres;
// the event just points at it.
type Event struct {
	Type      EventType `json:"type"`
	TradeID   string    `json:"tradeId"`
	UserID    string    `json:"userId"`
	SessionID string    `json:"sessionId"`
	OccurredAt time.Time `json:"occurredAt"`
}

// NewClient returns a *redis.Client wired from a redis:// URL.
// We set sane defaults: 5s connect timeout, 3 retries, pool of 10.
func NewClient(redisURL string) (*redis.Client, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	opt.DialTimeout = 5 * time.Second
	opt.ReadTimeout = 3 * time.Second
	opt.WriteTimeout = 3 * time.Second
	opt.MaxRetries = 3
	opt.PoolSize = 10
	return redis.NewClient(opt), nil
}

// EnsureGroup creates the consumer group if it doesn't exist. Safe to call
// repeatedly — a "BUSYGROUP" error means the group is already there, fine.
func EnsureGroup(ctx context.Context, rdb *redis.Client, stream, group string) error {
	// MKSTREAM = create the stream too if it's missing (otherwise XGROUP fails).
	// "$" = start consuming from the moment of group creation, ignoring history.
	err := rdb.XGroupCreateMkStream(ctx, stream, group, "$").Err()
	if err == nil {
		return nil
	}
	// "BUSYGROUP" means the group already exists — that's the desired state.
	if err.Error() == "BUSYGROUP Consumer Group name already exists" {
		return nil
	}
	return fmt.Errorf("ensure group %q on stream %q: %w", group, stream, err)
}
