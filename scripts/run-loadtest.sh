#!/usr/bin/env bash
# run-loadtest.sh — installs k6 if missing, runs the spec's 200-RPS / 60s
# load test against the local stack, and saves the HTML report + JSON summary
# to loadtest/results/.
#
# Use ON the EC2 instance for clean, no-network-noise numbers (loopback only),
# OR on your laptop with API_URL pointing at the public EC2 IP.
#
# Usage:
#   bash scripts/run-loadtest.sh                            # localhost:8080
#   API_URL=http://<ip>:8080 bash scripts/run-loadtest.sh   # remote target

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

OUT_DIR="loadtest/results"
mkdir -p "$OUT_DIR"

API_URL="${API_URL:-http://localhost:8080}"
echo "target: $API_URL"

# ── 1. Install k6 if needed ────────────────────────────────────────────────
if ! command -v k6 >/dev/null 2>&1; then
  echo "[install] k6 not found — fetching latest static binary..."
  TMP=$(mktemp -d)
  cd "$TMP"
  K6_VERSION="0.55.2"
  curl -sSL -o k6.tar.gz \
    "https://github.com/grafana/k6/releases/download/v${K6_VERSION}/k6-v${K6_VERSION}-linux-amd64.tar.gz"
  tar xzf k6.tar.gz
  sudo install -m 0755 "k6-v${K6_VERSION}-linux-amd64/k6" /usr/local/bin/k6
  cd "$REPO_ROOT"
  rm -rf "$TMP"
fi
echo "k6: $(k6 version | head -1)"

# ── 2. Verify target is reachable ──────────────────────────────────────────
if ! curl -sf "${API_URL}/health" >/dev/null; then
  echo "FATAL: ${API_URL}/health is not responding."
  echo "If you're targeting EC2, make sure security group allows TCP 8080."
  exit 1
fi

# ── 3. Run the load test ───────────────────────────────────────────────────
echo
echo "──────────────────────────────────────────────────────────"
echo " Running 200 RPS / 60 s load test (spec target)"
echo "──────────────────────────────────────────────────────────"
echo

# K6_WEB_DASHBOARD_EXPORT writes a self-contained HTML report you can
# commit + open in any browser.
API_URL="$API_URL" \
K6_WEB_DASHBOARD=true \
K6_WEB_DASHBOARD_EXPORT="$OUT_DIR/report.html" \
k6 run \
  --summary-export="$OUT_DIR/summary.json" \
  loadtest/k6.js | tee "$OUT_DIR/run.log"

echo
echo "──────────────────────────────────────────────────────────"
echo " ✓ load test complete"
echo
echo "   HTML report: $OUT_DIR/report.html"
echo "   JSON summary: $OUT_DIR/summary.json"
echo "   Raw log    : $OUT_DIR/run.log"
echo
echo " Quick verdict (from summary.json):"
python3 - "$OUT_DIR/summary.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
m = d.get("metrics", {})
def pull(name, agg):
    v = m.get(name, {}).get("values", {}).get(agg)
    return f"{v:.2f}" if isinstance(v,(int,float)) else "-"
print(f"   reqs total       : {pull('http_reqs','count')}")
print(f"   error rate       : {pull('http_req_failed','rate')}  (target < 0.01)")
print(f"   p50 (ms)         : {pull('http_req_duration','med')}")
print(f"   p95 (ms)         : {pull('http_req_duration','p(95)')}  (spec budget 150)")
print(f"   p99 (ms)         : {pull('http_req_duration','p(99)')}")
PY
echo "──────────────────────────────────────────────────────────"
