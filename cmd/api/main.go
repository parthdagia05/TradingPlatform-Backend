// Command api is the HTTP server.
//
// Boots in this order: load config, build logger, open Postgres + run
// migrations + (optionally) seed and backfill, open Redis + ensure consumer
// group, wire handlers and middleware, listen with graceful shutdown.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nevup/trade-journal/internal/auth"
	"github.com/nevup/trade-journal/internal/config"
	"github.com/nevup/trade-journal/internal/db"
	"github.com/nevup/trade-journal/internal/health"
	"github.com/nevup/trade-journal/internal/logger"
	"github.com/nevup/trade-journal/internal/metrics"
	mw "github.com/nevup/trade-journal/internal/middleware"
	"github.com/nevup/trade-journal/internal/queue"
	"github.com/nevup/trade-journal/internal/trades"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := logger.New(cfg.LogLevel).With("component", "api")
	log.Info("starting api",
		"port", cfg.Port, "logLevel", cfg.LogLevel, "seedOnStart", cfg.SeedOnStart)

	// boot work runs on a separate, time-bounded context so a misconfigured
	// dependency can't keep the container hanging forever
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer bootCancel()

	pool, err := db.NewPool(bootCtx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db pool: %w", err)
	}
	defer pool.Close()

	if err := db.RunMigrations(bootCtx, pool, cfg.MigrationsDir, log); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if cfg.SeedOnStart {
		if err := db.SeedFromCSV(bootCtx, pool, cfg.SeedFilePath, log); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
		// only backfill metrics if the tables are empty; once the worker has
		// touched them we leave live state alone
		hasSnap, err := metrics.HasSnapshots(bootCtx, pool)
		if err != nil {
			return fmt.Errorf("check snapshots: %w", err)
		}
		if !hasSnap {
			if err := metrics.BackfillFromTrades(bootCtx, pool, log); err != nil {
				return fmt.Errorf("backfill: %w", err)
			}
		}
	}

	rdb, err := queue.NewClient(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	defer rdb.Close()

	if err := queue.EnsureGroup(bootCtx, rdb, cfg.StreamName, cfg.ConsumerGroup); err != nil {
		return fmt.Errorf("ensure group: %w", err)
	}
	producer := queue.NewProducer(rdb, cfg.StreamName)
	// /health uses its own consumer handle to read XPENDING
	healthConsumer := queue.NewConsumer(rdb, cfg.StreamName, cfg.ConsumerGroup,
		cfg.ConsumerName+"-health")

	tradeRepo := trades.NewRepo(pool)
	metricRepo := metrics.NewRepo(pool)
	verifier := auth.NewVerifier(cfg.JWTSecret)

	tradeHandler := trades.NewHandler(tradeRepo, producer)
	metricHandler := metrics.NewHandler(metricRepo)
	healthHandler := health.NewHandler(pool, healthConsumer)

	r := chi.NewRouter()
	// trace before logger so log lines carry traceId; recoverer last so it
	// catches panics from anything earlier in the chain
	r.Use(mw.Trace)
	r.Use(mw.Logger(log))
	r.Use(mw.Recoverer(log))

	healthHandler.Mount(r)

	// landing page for anyone who lands on / in a browser (e.g. the HF Space UI)
	r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(landingHTML))
	})

	r.Group(func(r chi.Router) {
		r.Use(mw.Authenticator(verifier))
		// /trades has no userId in the path; the handler does the body-level
		// tenancy check itself
		tradeHandler.Mount(r)

		// /users/{userId}/* gets the path-level tenancy guard
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireUserMatch(func(req *http.Request) string {
				return chi.URLParam(req, "userId")
			}))
			metricHandler.Mount(r)
		})
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received, draining...")
	case err := <-errCh:
		return fmt.Errorf("listen: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Info("bye")
	return nil
}

const landingHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>NevUp Trade Journal - Track 1</title>
<style>
  :root { --bg:#fafafa; --fg:#222; --muted:#666; --accent:#0a58ca; --ok:#0a7d2c; --bad:#b93030; --card:#fff; }
  body { font: 14px/1.55 -apple-system, system-ui, "Segoe UI", sans-serif; max-width: 820px; margin: 3em auto; padding: 0 1em; color: var(--fg); background: var(--bg); }
  h1 { font-size: 1.5em; margin: 0 0 .3em; }
  h2 { font-size: 1.1em; margin-top: 1.6em; border-bottom: 1px solid #ddd; padding-bottom: 4px; }
  .lede { color: var(--muted); margin-top: 0; }
  code { background: #eef; padding: 1px 5px; border-radius: 3px; font-size: 13px; }
  table { border-collapse: collapse; margin: .8em 0; width: 100%; }
  th, td { text-align: left; padding: 6px 14px 6px 0; border-bottom: 1px solid #e6e6e6; }
  th { font-weight: 600; font-size: 13px; color: var(--muted); text-transform: uppercase; letter-spacing: .03em; }
  a { color: var(--accent); }
  .badges { display: flex; gap: 8px; flex-wrap: wrap; margin: 6px 0 18px; }
  .badge { display: inline-flex; align-items: center; gap: 6px; padding: 3px 10px; border-radius: 12px; font-size: 12px; font-weight: 600; background: #eee; color: #444; }
  .badge.ok { background: #e6f4ea; color: var(--ok); }
  .badge.bad { background: #fce8e8; color: var(--bad); }
  .badge.live::before { content: ""; display: inline-block; width: 8px; height: 8px; border-radius: 50%; background: currentColor; }
  .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); gap: 12px; margin: 1em 0; }
  .card { background: var(--card); border: 1px solid #e6e6e6; border-radius: 8px; padding: 14px; }
  .num { font-size: 1.6em; font-weight: 700; color: var(--ok); }
  .lab { font-size: 12px; color: var(--muted); text-transform: uppercase; letter-spacing: .05em; }
  pre { background: #1e1e2e; color: #eee; padding: 12px; border-radius: 6px; overflow: auto; font-size: 12.5px; }
  .row { display: flex; gap: 10px; align-items: center; flex-wrap: wrap; }
  button { font: inherit; padding: 6px 12px; border: 1px solid #ccc; border-radius: 5px; background: #fff; cursor: pointer; }
  button:hover { background: #f0f0f0; }
</style>
</head>
<body>
<h1>NevUp Trade Journal - Track 1</h1>
<p class="lede">System of Record: idempotent trade write API + async behavioural metrics. NevUp Hackathon 2026 backend submission.</p>
<div class="badges">
  <span id="health-badge" class="badge">checking...</span>
  <a class="badge" href="https://github.com/parthdagia05/TradingPlatform-Backend">repo</a>
  <a class="badge" href="/health">/health</a>
</div>

<h2>Verified throughput (k6, 200 RPS / 60 s)</h2>
<div class="grid">
  <div class="card"><div class="num">12,000</div><div class="lab">requests</div></div>
  <div class="card"><div class="num">0.00%</div><div class="lab">failure rate</div></div>
  <div class="card"><div class="num">2.8 ms</div><div class="lab">p95 latency</div></div>
  <div class="card"><div class="num">53x</div><div class="lab">under 150 ms budget</div></div>
</div>
<p class="lede">Run yourself with <code>docker compose up -d &amp;&amp; k6 run loadtest/k6.js</code>. Full HTML report committed at <code>loadtest/results/report.html</code>.</p>

<h2>Endpoints</h2>
<table>
  <thead><tr><th>Method</th><th>Path</th><th>Auth</th></tr></thead>
  <tbody>
    <tr><td>GET</td><td><a href="/health"><code>/health</code></a></td><td>none</td></tr>
    <tr><td>POST</td><td><code>/trades</code></td><td>JWT</td></tr>
    <tr><td>GET</td><td><code>/trades/{tradeId}</code></td><td>JWT + tenancy</td></tr>
    <tr><td>GET</td><td><code>/users/{userId}/metrics?from=&amp;to=&amp;granularity=</code></td><td>JWT + tenancy</td></tr>
  </tbody>
</table>

<h2>Try it (live)</h2>
<p class="lede">Issues a JWT for seed user Alex Mercer in your browser, hits the protected metrics endpoint, and displays the result.</p>
<div class="row">
  <button id="run-demo">GET /users/{alex}/metrics</button>
  <button id="run-403">try cross-tenant (must be 403)</button>
</div>
<pre id="demo-out">click a button above</pre>

<h2>Auth contract</h2>
<p>HS256 JWT, secret published in the kickoff PDF. <code>jwt.sub</code> must equal the <code>userId</code> in the request path or body, or the API returns <strong>403</strong> with a traceId-bearing JSON error - never 404. Missing or expired tokens return 401.</p>

<script>
const SECRET = "97791d4db2aa5f689c3cc39356ce35762f0a73aa70923039d8ef72a2840a1b02";
const ALEX = "f412f236-4edc-47a2-8f54-8763a6ed2ce8";
const JORDAN = "fcd434aa-2201-4060-aeb2-f44c77aa0683";

function b64url(buf) {
  return btoa(String.fromCharCode.apply(null, new Uint8Array(buf)))
    .replace(/=+$/,"").replace(/\+/g,"-").replace(/\//g,"_");
}
function b64urlString(s) {
  return btoa(s).replace(/=+$/,"").replace(/\+/g,"-").replace(/\//g,"_");
}
async function signJWT(sub) {
  const now = Math.floor(Date.now()/1000);
  const head = b64urlString(JSON.stringify({alg:"HS256",typ:"JWT"}));
  const body = b64urlString(JSON.stringify({sub:sub,iat:now,exp:now+86400,role:"trader"}));
  const data = head + "." + body;
  const k = await crypto.subtle.importKey("raw", new TextEncoder().encode(SECRET),
    {name:"HMAC",hash:"SHA-256"}, false, ["sign"]);
  const sig = await crypto.subtle.sign("HMAC", k, new TextEncoder().encode(data));
  return data + "." + b64url(sig);
}

async function refreshHealth() {
  const el = document.getElementById("health-badge");
  try {
    const r = await fetch("/health");
    const j = await r.json();
    el.className = "badge live ok";
    el.textContent = "live - db " + j.dbConnection + " - queue lag " + j.queueLag;
  } catch (e) {
    el.className = "badge live bad";
    el.textContent = "down";
  }
}
refreshHealth();
setInterval(refreshHealth, 30000);

document.getElementById("run-demo").onclick = async () => {
  const out = document.getElementById("demo-out");
  out.textContent = "calling...";
  const tok = await signJWT(ALEX);
  const url = "/users/" + ALEX + "/metrics?from=2025-01-01T00:00:00Z&to=2026-12-31T23:59:59Z&granularity=daily";
  const r = await fetch(url, { headers: { Authorization: "Bearer " + tok }});
  const j = await r.json();
  out.textContent = "GET " + url + "\nstatus: " + r.status + "\n\n" + JSON.stringify(j, null, 2);
};
document.getElementById("run-403").onclick = async () => {
  const out = document.getElementById("demo-out");
  out.textContent = "calling...";
  const tok = await signJWT(ALEX); // alex's token, jordan's url
  const url = "/users/" + JORDAN + "/metrics?from=2025-01-01T00:00:00Z&to=2026-12-31T23:59:59Z&granularity=daily";
  const r = await fetch(url, { headers: { Authorization: "Bearer " + tok }});
  const j = await r.json();
  out.textContent = "GET " + url + "  (with Alex's token)\nstatus: " + r.status + "\n\n" + JSON.stringify(j, null, 2);
};
</script>
</body>
</html>`
