# NevUp Trade Journal — Track 1 (System of Record)

Real-time trade journal engine with behavioural analytics for the
**NevUp Hiring Hackathon 2026**. Implements the full Track 1 spec:
idempotent write API, async metrics pipeline (Redis Streams), JWT
multi-tenancy, structured logging, k6 load test, OpenAPI 3.0 contract.

## Run it (one command)

```bash
docker compose up --build
```

This brings up four services:

> **No Docker?** A native dev path is included for low-resource machines:
> see [`No-Docker dev stack`](#no-docker-dev-stack) further below — it uses
> the system Postgres binaries plus an embedded `miniredis`, no containers.



| Service | Port | Role |
|---|---|---|
| `postgres` | 5432 | System of record |
| `redis` | 6379 | Stream queue + pending-count for `/health` |
| `api` | 8080 | HTTP server (this is what you call) |
| `worker` | — | Async metrics consumer |

On first boot the API loads `migrations/*.sql` then bulk-loads
`nevup_seed_dataset.csv` into Postgres via `COPY FROM`. Subsequent
`docker compose up` runs are no-ops on the seeder.

## Quick smoke test

```bash
# Health
curl -s http://localhost:8080/health | jq

# Issue a token for Alex Mercer (seed user)
TOKEN=$(node -e "
  const c = require('crypto');
  const SECRET = '97791d4db2aa5f689c3cc39356ce35762f0a73aa70923039d8ef72a2840a1b02';
  const b64 = (s) => Buffer.from(s).toString('base64')
    .replace(/=/g,'').replace(/\+/g,'-').replace(/\//g,'_');
  const now = Math.floor(Date.now()/1000);
  const h = b64(JSON.stringify({alg:'HS256',typ:'JWT'}));
  const p = b64(JSON.stringify({
    sub:'f412f236-4edc-47a2-8f54-8763a6ed2ce8',
    iat:now, exp:now+86400, role:'trader'
  }));
  const sig = c.createHmac('sha256',SECRET).update(\`\${h}.\${p}\`)
    .digest('base64').replace(/=/g,'').replace(/\+/g,'-').replace(/\//g,'_');
  console.log(\`\${h}.\${p}.\${sig}\`);
")

# Read Alex's metrics from the seed data
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/users/f412f236-4edc-47a2-8f54-8763a6ed2ce8/metrics?from=2025-01-01T00:00:00Z&to=2025-03-31T23:59:59Z&granularity=daily" \
  | jq

# Cross-tenant attempt (must be 403)
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/users/fcd434aa-2201-4060-aeb2-f44c77aa0683/metrics?from=2025-01-01T00:00:00Z&to=2025-03-31T23:59:59Z&granularity=daily"
# → 403
```

## Endpoints

| Method | Path | Purpose | Auth |
|---|---|---|---|
| `POST` | `/trades` | Submit (idempotent on `tradeId`) | JWT |
| `GET` | `/trades/{tradeId}` | Fetch a trade | JWT + tenancy |
| `GET` | `/users/{userId}/metrics` | Behavioural metrics + timeseries | JWT + tenancy |
| `GET` | `/health` | DB + queue state | none |

Full schema: [`openapi.yaml`](openapi.yaml).

## The 5 behavioural metrics

| # | Metric | When it runs |
|---|---|---|
| 1 | Plan Adherence Score | every `trade.closed` |
| 2 | Revenge Trade Flag | every `trade.opened` |
| 3 | Session Tilt Index | every `trade.closed` |
| 4 | Win Rate by Emotional State | every `trade.closed` |
| 5 | Overtrading Detector | every `trade.opened` |

Each lives in [`internal/metrics/<name>.go`](internal/metrics). The async
worker ([`cmd/worker`](cmd/worker)) consumes the Redis stream and dispatches
to the right calculator. **HTTP write path never blocks on this work.**

## Auth

HS256 JWT. Secret published in the kickoff PDF, also in
[`docker-compose.yml`](docker-compose.yml) (and `.env.example`). Tokens carry
`sub` (=userId), `iat`, `exp`, `role: "trader"`. The middleware enforces:

* No `Authorization` header → **401**
* Bad signature, expired, malformed → **401**
* `jwt.sub != requestedUserId` → **403** (never 404)

See [`internal/auth`](internal/auth) and [`internal/middleware/auth.go`](internal/middleware/auth.go).

## Logging

Every request emits one structured JSON line with the spec's exact fields:

```json
{"time":"...","level":"INFO","msg":"request",
 "traceId":"...","userId":"...","method":"POST","path":"/trades",
 "statusCode":200,"latency":47}
```

Errors include the `traceId` in the response body so client and server logs
can be correlated.

## Testing

```bash
# unit (no external deps)
go test -short ./...

# integration (needs the stack up)
docker compose up -d
go test ./tests/...
```

Coverage:

* `internal/auth/jwt_test.go` — token issue + verify, signature/malformed rejection
* `tests/integration_test.go`:
  * **Idempotency proof** — POST same trade twice, both 200, same `createdAt`
  * **403 cross-tenant proof** — User A's token requesting User B's metrics → 403
  * **401 unauth proof** — missing header → 401
  * **/health** — exposes DB connection + queue lag

## Load test

```bash
docker compose up -d
mkdir -p loadtest/results
k6 run --summary-export=loadtest/results/summary.json loadtest/k6.js
```

Spec target: 200 RPS for 60 seconds, p95 ≤ 150 ms. The k6 thresholds gate
the run — non-zero exit on violation. See [`loadtest/README.md`](loadtest/README.md).

## Project layout

```
cmd/
  api/          HTTP server binary
  worker/       Async consumer binary
internal/
  config/       Env-var loader
  logger/       slog setup
  db/           pgx pool, migration runner, CSV seeder
  auth/         JWT verify + issuer
  middleware/   trace-id, recoverer, request logger, JWT auth
  httpx/        shared HTTP plumbing (error envelope, context keys)
  queue/        Redis Streams producer + consumer
  trades/       POST /trades + GET /trades/{id}
  metrics/      5 metric calculators + repo + GET /users/{id}/metrics
  health/       GET /health
  worker/       async pipeline orchestrator
migrations/     0001_init.up.sql, 0002_metrics_tables.up.sql, .down counterparts
loadtest/       k6 script + run docs
tests/          integration tests
openapi.yaml    OpenAPI 3.0 contract
DECISIONS.md    architectural rationale
```

## Where to read next

* [DECISIONS.md](DECISIONS.md) — every architectural choice and why
* [openapi.yaml](openapi.yaml) — exact request/response shapes
* [internal/middleware/auth.go](internal/middleware/auth.go) — the 403 rule
* [internal/trades/repo.go](internal/trades/repo.go) — idempotent insert
* [internal/metrics/](internal/metrics) — the 5 calculators

## Verify the Docker stack without Docker

Two free options for proving `docker compose up` works on a green machine:

### GitHub Actions

A workflow at [`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs on
every push and PR. It:

1. Static-checks the code (`go build`, `go vet`, `go test -short`).
2. Lints the Dockerfile with hadolint.
3. Builds the image and `docker compose up`s the full stack.
4. Smoke-tests `/health` and runs all 4 integration tests.

A green check on a public repo *is* the proof. Free for public repos.

### GitHub Codespaces

A devcontainer at [`.devcontainer/devcontainer.json`](.devcontainer/devcontainer.json)
gives you a browser-based VS Code with Docker preinstalled. Open the repo in
a Codespace and run `docker compose up --build`. 60 hours/month free.

## No-Docker dev stack

For machines that can't run Docker (low disk, no privileges, etc.), the same
project boots natively in two commands:

```bash
# 1. Start a user-owned Postgres on /tmp:5433 (uses system PG 16 binaries)
./scripts/local-stack.sh up

# 2. Run the API + worker + an embedded miniredis in one process
DATABASE_URL='postgres://nevup@/nevup?host=/tmp&port=5433&sslmode=disable' \
JWT_SECRET='97791d4db2aa5f689c3cc39356ce35762f0a73aa70923039d8ef72a2840a1b02' \
SEED_FILE_PATH=./nevup_seed_dataset.csv \
MIGRATIONS_DIR=./migrations \
SEED_ON_START=true \
PORT=8080 REDIS_PORT=6380 LOG_LEVEL=info \
STREAM_NAME=trade-events CONSUMER_GROUP=metrics-workers CONSUMER_NAME=worker-1 \
go run ./cmd/devstack
```

The `cmd/devstack` binary is excluded from the production Dockerfile —
it exists purely for local testing. Integration tests work unchanged:

```bash
API_URL=http://localhost:8080 go test ./tests/...
```

When you're done:

```bash
./scripts/local-stack.sh down   # stops PG, removes the cluster directory
```

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `db pool: connect: ...` on boot | Postgres healthcheck not yet green | Wait 5s; compose retries |
| `seed skipped — trades already populated` | Re-running on a non-empty DB | Expected; idempotent by design |
| Tests fail with "connection refused" | API not running | `docker compose up -d` first |
| `BUSYGROUP` warning | Worker already created the consumer group | Harmless; we ignore it |
