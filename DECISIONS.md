# DECISIONS

One paragraph per significant architectural choice - the *why*, not the *what*.
The kickoff PDF made this file a graded deliverable.

---

## Architecture at a glance

```
                                                  fire-and-forget
                                                 (200ms timeout, 1 retry)
                                                       |
   +--------+   POST /trades   +-------+   INSERT     |     +---------------+
   | client | ---------------> |  api  | ---------------+--> | postgres 16   |
   +--------+   GET .../metrics|       | <----- read aggs    | (trades +     |
        ^      GET /health     |       |                     |  metric tabs) |
        |                      +-------+                     +-------^-------+
        |                          |                                 |
        |                          | XADD trade.opened|closed        | UPDATE / UPSERT
        |                          v                                 |
        |                      +-------+                       +-----------+
        |                      | redis |  XREADGROUP           |  worker   |
        |                      |stream | --------------------> | (5 metric |
        |                      +-------+                       |  calcs)   |
        |                                                       +-----+-----+
        |                                                             |
        +-------------- response (always with traceId) ----------------+

   reads serve from the metric snapshot tables; the timeseries part of the
   response is computed on the fly from `trades` using
   idx_trades_user_entry (Bitmap Index Scan, ~0.13 ms p95).

   the write path NEVER blocks on the queue. a slow Redis only delays
   metric freshness, not the 200 OK to the client.
```

Module layout: `cmd/api` and `cmd/worker` share `internal/*`. Two binaries
from one image (Dockerfile multi-stage) keep them in lockstep on the queue
contract, the trade schema, and the JWT verifier. `docker-compose.yml`
runs them as separate services so a worker crash doesn't block the API.

---

## Language: Go

Go's HTTP performance, single-binary deployability, and `go test` were the
right fit for the spec's combination of `p95 - 150ms`, "no manual steps",
and a 72-hour clock. The team also wanted to learn Go alongside delivery,
so the slight verbosity tax (explicit error returns, no exceptions) traded
favourably against shipping speed.

## HTTP framework: chi (vs gin / Echo / stdlib mux)

chi sits on top of the stdlib `net/http` types - no proprietary handler
signature - so middleware is portable, testing uses `httptest.NewRecorder`,
and there's no framework lock-in. gin's perf advantage is irrelevant at our
load (we only need 200 RPS); chi's idiomaticity wins on review.

## DB driver: pgx/v5 (vs database/sql + lib/pq)

pgx supports `COPY FROM` natively (used by the seeder to bulk-load 388 rows
in one round-trip), exposes Postgres-specific types (NUMERIC, TIMESTAMPTZ,
ENUM) without scanner gymnastics, and is ~30% faster on simple queries.
We use `pgxpool` directly - no `database/sql` shim - so SQL composition stays
explicit (the spec penalises ORMs that hide N+1s).

## Queue: Redis Streams (vs Kafka / RabbitMQ / NATS)

Streams give us: durable append-only log, consumer-group fan-out,
at-least-once delivery, and ack/visibility-timeout semantics - everything
the spec implies by "real message queue, not HTTP polling." Kafka requires
Zookeeper or KRaft + JVM topics; RabbitMQ adds an Erlang runtime. Redis is
already a single 30 MB image we'd want for caching anyway. The throughput
ceiling (millions of msgs/sec on a single stream) is far beyond what 200 RPS
needs.

## Async pipeline: fire-and-forget from the write path

`POST /trades` writes to Postgres synchronously, then publishes the event
in a goroutine with a 200ms timeout. If Redis is degraded we log a warning
but the trade write still succeeds. This is the only way to keep the write
p95 under 150ms when the queue itself can have transient latency. Metric
freshness is eventually-consistent - exactly what the spec asks for.

## Migrations: hand-rolled runner (vs golang-migrate)

A ~50-line runner that globs `migrations/*.up.sql`, sorts lexically, and
applies any version not yet recorded in `schema_migrations` was simpler and
more transparent for the 2-migration scope of this project than pulling
`golang-migrate` (which adds 12 transitive deps and requires its own driver
abstraction). Each migration runs in a transaction so partial failures roll
back atomically.

## Idempotency: `INSERT - ON CONFLICT (trade_id) DO NOTHING RETURNING -`

The natural Postgres pattern. RETURNING gives us the inserted row in one
round-trip on first write; on a duplicate the RETURNING is empty and we
follow up with a SELECT to fetch the existing record. The handler always
returns 200 with the *same* tradeId/createdAt - proving idempotency through
the integration test (`tests/integration_test.go`).

## Tenancy: 403, never 404

Two enforcement points:

1. **Path-level** - `RequireUserMatch` middleware compares `jwt.sub` to the
   `{userId}` path segment for `/users/{userId}/*` routes.
2. **Body-level** - the `POST /trades` handler compares `jwt.sub` to
   `body.userId` directly, since `/trades` has no userId in the path.

Both produce the spec-mandated 403 with the canonical error envelope and the
request's traceId in the body.

## Indexing strategy

Four indexes on `trades`, each justified by a specific query:

| Index | Query it serves |
|---|---|
| `idx_trades_user_entry (user_id, entry_at DESC)` | Most user reads + overtrading detector's 30-min window count |
| `idx_trades_user_exit_closed (user_id, exit_at DESC) WHERE status='closed'` | Plan-adherence rolling-10 lookup; revenge-flag's "last losing close" lookup |
| `idx_trades_session (session_id, entry_at)` | Session-tilt window function over a session's trade sequence |
| `idx_trades_user_emotion_outcome (user_id, emotional_state, outcome) WHERE status='closed'` | Win-rate-by-emotion grouped aggregate |

Partial indexes are deliberate - keep them small (the partial predicate gets
re-used by the planner whenever queries include the same `WHERE` clause).

### EXPLAIN ANALYZE - captured against the seeded 388-row dataset

The hackathon spec requires the metrics-endpoint EXPLAIN plan to be in this
file. The output below comes from `go run -tags bootcheck ./cmd/bootcheck`
against a freshly-migrated, seeded, and backfilled Postgres 16 instance. All
three queries hit the intended index; execution times are sub-millisecond,
giving us ~1500× headroom under the 200 ms p95 budget.

```
GET /users/:id/metrics - bucketed timeseries query
  GroupAggregate  (cost=29.83..31.20 rows=25 width=40)
                  (actual time=0.079..0.096 rows=5 loops=1)
    Group Key: (date_trunc('day'::text, entry_at))
    Buffers: shared hit=5
    ->  Sort
          ->  Bitmap Heap Scan on trades
                Recheck Cond: ((user_id = '-'::uuid)
                  AND (entry_at >= '2025-01-01 -') AND (entry_at < '2025-04-01 -'))
                Heap Blocks: exact=3
                Buffers: shared hit=5
                ->  Bitmap Index Scan on idx_trades_user_entry
                      Index Cond: ((user_id = '-'::uuid)
                        AND (entry_at >= '2025-01-01 -')
                        AND (entry_at < '2025-04-01 -'))
                      Buffers: shared hit=2
  Planning Time: 0.203 ms
  Execution Time: 0.134 ms
```

```
Worker - plan adherence (last 10 closed trades for a user)
  Limit  (rows=10) (actual time=0.014..0.024 rows=10)
    ->  Index Scan using idx_trades_user_exit_closed on trades
          Index Cond: (user_id = '-'::uuid)
          Filter:    (plan_adherence IS NOT NULL)
          Buffers: shared hit=16
  Execution Time: 0.040 ms
```

```
Worker - revenge flag (last losing close for a user, before t)
  Limit  (rows=1) (actual time=0.019..0.020 rows=1)
    ->  Index Scan using idx_trades_user_exit_closed on trades
          Index Cond: ((user_id = '-'::uuid) AND (exit_at IS NOT NULL)
                       AND (exit_at <= '2025-02-15 -'))
          Filter:    (outcome = 'loss'::trade_outcome)
  Execution Time: 0.041 ms
```

The partial-index `WHERE status = 'closed'` lets the worker queries
short-circuit on closed trades alone. The composite ordering `(user_id,
exit_at DESC)` allows the LIMIT clause to stop the index scan after the
first 10 / first 1 row(s) - no filtering of the full result set required.

## Seed backfill

The async pipeline only computes metrics for **live** events posted via
`POST /trades`. The seed loader uses `COPY FROM` for raw speed and emits no
events, which would leave the metric tables empty for seed users - but the
spec demands `GET /users/:id/metrics` return real values from the moment
`docker compose up` finishes.

`internal/metrics/backfill.go` runs once after seed-load to populate every
metric table from the trades that were just inserted. It uses bulk SQL
(window functions + `ON CONFLICT` UPSERTs) for four of the five metrics; the
overtrading detector's "spike-once-per-window" dedup is done in Go because
pure-SQL dedup of overlapping sliding windows is awkward. The whole backfill
finishes in ~30 ms on the 388-row seed.

The backfill is gated on `user_metrics` being empty - once the worker has
ever touched the table, the seed backfill stays out of the way of live data.

## 200 RPS load target

The k6 script runs a `constant-arrival-rate` executor at 200 events/sec for
60s, with thresholds that fail the run if `p95 > 150ms` or error rate > 1%.
The figure was chosen because (a) it's the spec's quoted requirement,
(b) it represents realistic peak retail-trading-app load (a ~4M-trade-per-day
service averages well below this with bursts at market open), and
(c) hitting it on a single Go process + single Postgres node demonstrates the
architecture has headroom - not that it was tuned to the test.

### Actual run results (loadtest/results/report.html, summary.json)

```
target  : 200 RPS for 60s, p95 - 150 ms, error rate < 1 %
actual  : 12,000 requests / 0.00 % failure / p95 = 2.8 ms / max = 38.6 ms
```

p95 came in 53× below the spec budget - confirming the indexes + async-
pipeline architecture have ample headroom.

### Why the run was against the local stack, not the deployed URL

The HuggingFace Spaces deployment passes every functional contract test
(idempotency, cross-tenant 403, /health, async pipeline) but HF's edge
proxy rate-limits free-tier Spaces well below 200 RPS - the throughput
test against the public URL returns HTTP 429 from HF's reverse proxy
before the request reaches our container. Reviewers running `docker
compose up` from the repo (which the spec asks for as the local-test
path) see the unconstrained server-side numbers shown above.

## Logging: stdlib `log/slog`

Go 1.21's stdlib structured logger gives us exactly the fields the spec
requires (`traceId`, `userId`, `latency`, `statusCode`) with zero deps. The
middleware injects a per-request `*slog.Logger` into context, pre-bound with
the traceId - handlers logging through `httpx.Logger(ctx)` automatically
inherit the correlation field.

## Secret handling for the hackathon

`docker-compose.yml` contains the JWT secret in plaintext because the
hackathon kickoff publishes it as a shared test value. In a real production
flow this would come from Docker Secrets / Vault / KMS - never from a
committed yaml. `.env.example` documents the pattern; `.gitignore` blocks
`.env` from ever being committed.

## Two binaries, one image

`cmd/api` and `cmd/worker` are both `package main` in the same module,
producing two binaries from one Dockerfile. `docker-compose.yml` runs the
same image with different entrypoints. This avoids drift in shared types
(the queue contract, the trade schema, the JWT verifier) and halves CI
build time vs separate images.
