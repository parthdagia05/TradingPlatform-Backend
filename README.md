# NevUp Trade Journal - Track 1

[![CI](https://github.com/parthdagia05/TradingPlatform-Backend/actions/workflows/ci.yml/badge.svg)](https://github.com/parthdagia05/TradingPlatform-Backend/actions/workflows/ci.yml)

NevUp Hiring Hackathon 2026 - Track 1 (System of Record).

A real-time trade journal with idempotent writes, async metrics, JWT
multi-tenancy, and a 200 RPS load-tested write path.

- live URL: https://parthdagia-tradingplatform-backend.hf.space
- k6 load-test report: https://parthdagia-tradingplatform-backend.hf.space/loadtest/report.html

## Run it

```bash
docker compose up --build
```

That's it. Postgres + Redis + API + worker come up healthy, the seed CSV
loads, and the metric backfill runs - no manual steps. The API listens on
`http://localhost:8080`.

| Service  | Port  | Role                              |
|----------|-------|-----------------------------------|
| postgres | 5432  | system of record                  |
| redis    | 6379  | streams queue + pending count     |
| api      | 8080  | http server (this is what you hit)|
| worker   | -     | async metrics consumer            |

## Endpoints

| Method | Path                          | Auth          |
|--------|-------------------------------|---------------|
| GET    | `/health`                     | none          |
| POST   | `/trades`                     | JWT           |
| GET    | `/trades/{tradeId}`           | JWT + tenancy |
| GET    | `/users/{userId}/metrics`     | JWT + tenancy |

Full schema: [openapi.yaml](openapi.yaml).

`GET /users/{userId}/metrics` accepts `from`, `to`, `granularity`
(`hourly | daily | rolling30d`).

## Smoke test

```bash
# health (no auth)
curl -s http://localhost:8080/health | jq

# issue a token for Alex Mercer (a seed user)
TOKEN=$(node -e "
  const c=require('crypto'), S='97791d4db2aa5f689c3cc39356ce35762f0a73aa70923039d8ef72a2840a1b02';
  const b=s=>Buffer.from(s).toString('base64').replace(/=/g,'').replace(/\+/g,'-').replace(/\//g,'_');
  const n=Math.floor(Date.now()/1000);
  const h=b(JSON.stringify({alg:'HS256',typ:'JWT'}));
  const p=b(JSON.stringify({sub:'f412f236-4edc-47a2-8f54-8763a6ed2ce8',iat:n,exp:n+86400,role:'trader'}));
  console.log(h+'.'+p+'.'+c.createHmac('sha256',S).update(h+'.'+p).digest('base64')
    .replace(/=/g,'').replace(/\+/g,'-').replace(/\//g,'_'));
")

# read Alex's metrics
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/users/f412f236-4edc-47a2-8f54-8763a6ed2ce8/metrics?from=2025-01-01T00:00:00Z&to=2026-12-31T23:59:59Z&granularity=daily" | jq

# cross-tenant attempt - must be 403
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/users/fcd434aa-2201-4060-aeb2-f44c77aa0683/metrics?from=2025-01-01T00:00:00Z&to=2026-12-31T23:59:59Z&granularity=daily"
# -> 403
```

## The five behavioural metrics

Each lives in its own file under [internal/metrics/](internal/metrics).
The async worker consumes Redis Stream events and dispatches to the right
calculator. The HTTP write path never blocks on this work.

1. plan adherence - rolling 10-trade average per user
2. revenge flag - opens within 90s of a losing close + anxious/fearful
3. session tilt index - loss-following / total trades in the session
4. win rate by emotional state
5. overtrading detector - more than 10 trades in any 30-min sliding window

Seed data is backfilled into the metric tables on first boot
([internal/metrics/backfill.go](internal/metrics/backfill.go)) so
`GET /users/:id/metrics` returns real values immediately after
`docker compose up`.

## Auth

HS256 JWT. Secret is the one published in the kickoff PDF. Tokens carry
`sub` (=userId), `iat`, `exp`, `role: "trader"`.

- missing Authorization header -> 401
- bad signature, expired, malformed -> 401
- `jwt.sub != requestedUserId` -> **403**, never 404

See [internal/auth/jwt.go](internal/auth/jwt.go) and
[internal/middleware/auth.go](internal/middleware/auth.go).

## Logging

Every request emits one structured JSON line:

```json
{"time":"...","level":"INFO","msg":"request",
 "traceId":"...","userId":"...","method":"POST","path":"/trades",
 "statusCode":200,"latency":47}
```

Errors include the `traceId` in the response body so client and server
logs correlate.

## Testing

```bash
# unit tests, no external deps
go test -short ./...

# integration tests against the live stack
docker compose up -d
go test ./tests/...
```

The integration suite proves the spec's must-have contracts:

- `TestIdempotentTradeWrite` - POST same trade twice, both 200, same record
- `TestCrossTenantReturns403` - User A's token reading User B -> 403
- `TestUnauthenticatedReturns401` - missing header -> 401
- `TestHealthReturnsState` - `/health` exposes db + queue lag

## Load test

```bash
docker compose up -d
mkdir -p loadtest/results
k6 run --summary-export=loadtest/results/summary.json loadtest/k6.js
```

Spec target is 200 RPS for 60s with p95 <= 150ms. The committed
[loadtest/results/report.html](loadtest/results/report.html) shows
**p95 = 2.8 ms across 12,000 requests, 0% failures**. See `DECISIONS.md`
for the run methodology.

## Layout

```
cmd/
  api/         http server
  worker/      async metrics consumer
internal/
  auth/        jwt verify + issuer
  config/      env loader
  db/          pgx pool, migration runner, csv seeder
  health/      GET /health
  httpx/       error envelope, context keys
  logger/      slog setup
  metrics/     5 calculators + backfill + GET /users/:id/metrics
  middleware/  trace, request log, recoverer, jwt, tenancy
  queue/       redis streams producer + consumer
  trades/      POST /trades + GET /trades/{id}
  worker/      async pipeline orchestrator
migrations/    0001 + 0002 (up + down)
loadtest/      k6 script + html report + summary
tests/         integration tests
openapi.yaml   openapi 3.0 contract
DECISIONS.md   architectural rationale + EXPLAIN plan
```
