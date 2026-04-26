package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Message wraps a single stream entry pulled by a consumer.
// Ack() must be called once the message has been processed successfully -
// unacked messages are redelivered to another consumer after the visibility
// timeout (5s here), giving us at-least-once delivery semantics.
type Message struct {
	ID     string
	Stream string
	Group  string
	Event  Event
	rdb    *redis.Client
}

// Ack tells Redis "I processed this; don't redeliver." Idempotent.
func (m *Message) Ack(ctx context.Context) error {
	return m.rdb.XAck(ctx, m.Stream, m.Group, m.ID).Err()
}

// Consumer reads events off a Redis stream as part of a consumer group.
// Multiple Consumer instances with the same Group name share the load:
// each message goes to exactly one consumer in the group. Different
// ConsumerName values must be unique within the group.
type Consumer struct {
	rdb          *redis.Client
	stream       string
	group        string
	consumerName string
}

// NewConsumer creates a Consumer. EnsureGroup must already have been called.
func NewConsumer(rdb *redis.Client, stream, group, consumerName string) *Consumer {
	return &Consumer{rdb: rdb, stream: stream, group: group, consumerName: consumerName}
}

// QueueLag returns the number of pending (delivered but un-acked) messages
// for the consumer group. Used by /health to expose backlog.
func (c *Consumer) QueueLag(ctx context.Context) (int64, error) {
	res, err := c.rdb.XPending(ctx, c.stream, c.group).Result()
	if err != nil {
		// If the group doesn't exist yet (cold start), lag = 0 not an error.
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, fmt.Errorf("xpending: %w", err)
	}
	return res.Count, nil
}

// Read returns up to `count` messages. If `block > 0` the server holds the
// connection until either a message arrives or the block window expires;
// `block = 0` is a non-blocking poll that returns empty if nothing's queued.
//
// The caller (Worker.Run) is expected to sleep briefly on empty results when
// using non-blocking mode, otherwise it would spin. Non-blocking mode is the
// safe default - it works against real Redis, miniredis, and behind any
// proxy/load-balancer that might cap idle connection time.
func (c *Consumer) Read(ctx context.Context, count int64, block time.Duration) ([]Message, error) {
	res, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    c.group,
		Consumer: c.consumerName,
		Streams:  []string{c.stream, ">"},
		Count:    count,
		Block:    block,
	}).Result()
	if err != nil {
		// redis.Nil = no messages within block window. Normal - return empty.
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, fmt.Errorf("xreadgroup: %w", err)
	}

	var out []Message
	for _, s := range res {
		for _, m := range s.Messages {
			ev, err := decodeValues(m.Values)
			if err != nil {
				// Bad message - ack so it doesn't loop forever, log to drop floor.
				_ = c.rdb.XAck(ctx, c.stream, c.group, m.ID).Err()
				continue
			}
			out = append(out, Message{
				ID:     m.ID,
				Stream: c.stream,
				Group:  c.group,
				Event:  ev,
				rdb:    c.rdb,
			})
		}
	}
	return out, nil
}

func decodeValues(v map[string]any) (Event, error) {
	get := func(k string) string {
		if s, ok := v[k].(string); ok {
			return s
		}
		return ""
	}
	t, err := time.Parse("2006-01-02T15:04:05.000Z07:00", get("occurredAt"))
	if err != nil {
		return Event{}, fmt.Errorf("parse occurredAt %q: %w", get("occurredAt"), err)
	}
	return Event{
		Type:       EventType(get("type")),
		TradeID:    get("tradeId"),
		UserID:     get("userId"),
		SessionID:  get("sessionId"),
		OccurredAt: t,
	}, nil
}
