---
title: NevUp Trade Journal API
emoji: 📊
colorFrom: blue
colorTo: indigo
sdk: docker
app_port: 7860
pinned: false
license: mit
---

# NevUp Track 1 - System of Record (live)

Live deployment for the **NevUp Hiring Hackathon 2026, Track 1 (Backend Engineering)**
submission. The full source code, OpenAPI specification, k6 load test, EXPLAIN
plans, and architectural rationale (DECISIONS.md) are at:

**https://github.com/parthdagia05/TradingPlatform-Backend**

This Space bundles all four services (Postgres, Redis, API, async worker) into
a single container because HuggingFace Spaces only runs one image. The repo's
`docker-compose.yml` is the canonical multi-service deployment - what reviewers
test locally - and remains unchanged.

## Live endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET`  | `/health` | none | DB connection + queue lag |
| `POST` | `/trades` | JWT | submit a trade (idempotent on `tradeId`) |
| `GET`  | `/trades/{tradeId}` | JWT + tenancy | fetch a trade |
| `GET`  | `/users/{userId}/metrics` | JWT + tenancy | behavioural metrics + timeseries |

## Quick smoke test

```bash
curl https://<this-space>.hf.space/health
```

Expected: `{"status":"ok","dbConnection":"connected","queueLag":0,...}`

## Auth

HS256 JWT, secret `97791d4db2aa5f689c3cc39356ce35762f0a73aa70923039d8ef72a2840a1b02`
(the value published in the kickoff PDF). See the GitHub README for a token-
generation snippet and the cross-tenant 403 contract.

## Behavioural metrics

The async worker, consuming events off a Redis Stream, computes the five
metrics the spec mandates: plan adherence (rolling 10), revenge flag
(90 s post-loss + anxious/fearful), session tilt index, win rate by emotional
state, and overtrading detector (>10 trades in any 30-min sliding window).
Seed-time backfill populates the metric tables from the 388 trade CSV the
moment the container boots, so `GET /users/:id/metrics` returns real values
on first request.
