#!/usr/bin/env bash
# ec2-bootstrap.sh — provision a fresh Ubuntu 24.04 EC2 host and bring up the
# full Track 1 stack via docker-compose.
#
# Idempotent: safe to re-run. If Docker is already installed it skips that step.
# If the stack is already up it just reports status.
#
# Usage (on the EC2 instance, after `git clone`):
#   cd TradingPlatform-Backend
#   bash scripts/ec2-bootstrap.sh
#
# Verifies success by polling /health for up to 3 minutes.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

echo "──────────────────────────────────────────────────────────"
echo " NevUp Track 1 — EC2 deploy"
echo "──────────────────────────────────────────────────────────"

# ── 1. Install Docker + compose plugin if missing ──────────────────────────
# Ubuntu's compose plugin package is "docker-compose-v2" (universe).
# Docker's own apt repo calls the same thing "docker-compose-plugin".
# We try both so the script works on either repo source.
if ! command -v docker >/dev/null 2>&1; then
  echo "[1/4] Installing Docker..."
  sudo apt-get update -y
  sudo apt-get install -y --no-install-recommends docker.io
  sudo systemctl enable --now docker
else
  echo "[1/4] Docker already installed: $(docker --version)"
fi
if ! docker compose version >/dev/null 2>&1; then
  echo "       Installing compose plugin..."
  if apt-cache show docker-compose-v2 >/dev/null 2>&1; then
    sudo apt-get install -y --no-install-recommends docker-compose-v2
  elif apt-cache show docker-compose-plugin >/dev/null 2>&1; then
    sudo apt-get install -y --no-install-recommends docker-compose-plugin
  else
    echo "       Neither package available; installing via curl..."
    sudo mkdir -p /usr/local/lib/docker/cli-plugins
    sudo curl -sSL \
      https://github.com/docker/compose/releases/latest/download/docker-compose-linux-x86_64 \
      -o /usr/local/lib/docker/cli-plugins/docker-compose
    sudo chmod +x /usr/local/lib/docker/cli-plugins/docker-compose
  fi
fi
echo "       compose: $(docker compose version 2>&1 | head -1)"

# Add the current user to the docker group (takes effect next login).
if ! id -nG "$USER" | grep -qw docker; then
  sudo usermod -aG docker "$USER"
  echo "       (added $USER to docker group — re-login takes effect)"
fi

# ── 2. Build + start the stack ─────────────────────────────────────────────
echo "[2/4] Bringing up the stack (this takes ~2-3 min on first run)..."
sudo docker compose up -d --build --wait --wait-timeout 180

# ── 3. Show service status ─────────────────────────────────────────────────
echo "[3/4] Stack status:"
sudo docker compose ps

# ── 4. Verify /health ──────────────────────────────────────────────────────
echo "[4/4] Smoke-testing /health..."
for i in $(seq 1 30); do
  if curl -sf http://localhost:8080/health >/dev/null; then
    body=$(curl -s http://localhost:8080/health)
    echo "       /health → 200 OK"
    echo "       body: $body"
    break
  fi
  if [ "$i" -eq 30 ]; then
    echo "       /health never came up — dumping logs"
    sudo docker compose logs --tail=80 api
    exit 1
  fi
  sleep 2
done

PUBIP=$(curl -s --max-time 4 http://169.254.169.254/latest/meta-data/public-ipv4 \
        || curl -s ifconfig.me)
echo
echo "──────────────────────────────────────────────────────────"
echo " ✓ Deploy complete."
echo
echo "   Local: http://localhost:8080/health"
echo "   Public: http://${PUBIP}:8080/health"
echo
echo "   Make sure the EC2 security group allows TCP 8080 from 0.0.0.0/0."
echo "──────────────────────────────────────────────────────────"
