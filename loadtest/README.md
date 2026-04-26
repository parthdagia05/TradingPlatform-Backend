# Load test

Proves the spec's throughput + latency budget:

> Sustain 200 concurrent trade-close events/sec for 60 seconds with p95 write
> latency ≤ 150ms.

## Run

Bring the stack up first:

```bash
docker compose up -d
```

Then with [k6](https://k6.io) installed:

```bash
mkdir -p loadtest/results
k6 run \
  --summary-export=loadtest/results/summary.json \
  --out json=loadtest/results/run.json \
  loadtest/k6.js
```

For an HTML report (k6 v0.49+):

```bash
K6_WEB_DASHBOARD=true \
K6_WEB_DASHBOARD_EXPORT=loadtest/results/report.html \
k6 run loadtest/k6.js
```

## Pass criteria

The thresholds inside `k6.js` gate the run:

* `http_req_failed` rate < 1%
* `http_req_duration` p95 < 150 ms

Any threshold violation makes k6 exit non-zero — handy for CI.

## Environment

| Var | Default | Purpose |
|---|---|---|
| `API_URL` | `http://localhost:8080` | API base URL |
| `JWT_SECRET` | hackathon spec secret | HMAC signing key |
