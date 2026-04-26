#!/usr/bin/env bash
# deploy-hfspaces.sh — push this repo to a HuggingFace Space.
#
# Required env vars:
#   HF_USERNAME   your HF username (e.g. parthdagia05)
#   HF_SPACE      the HF Space name (e.g. tradingplatform-backend)
#   HF_TOKEN      a write-scope HF access token
#                 (huggingface.co/settings/tokens → New token → type "Write")
#
# Usage:
#   HF_USERNAME=parthdagia05 \
#   HF_SPACE=tradingplatform-backend \
#   HF_TOKEN=hf_xxxxxxxx \
#   bash scripts/deploy-hfspaces.sh
#
# What it does:
#   1. Clones the (existing, empty) HF Space repo to a temp dir.
#   2. rsyncs this entire repo into it (excluding .git, CI, results, tmp).
#   3. Replaces the root Dockerfile + README.md with the HF-bundle versions
#      from deploy/hfspaces/.
#   4. Commits + pushes to HF git, which triggers the build on HF.
#
# The HF Space build then takes ~3-5 min and exposes the service at
#   https://${HF_USERNAME}-${HF_SPACE}.hf.space

set -euo pipefail

: "${HF_USERNAME:?HF_USERNAME is required}"
: "${HF_SPACE:?HF_SPACE is required}"
: "${HF_TOKEN:?HF_TOKEN is required}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

WORK=$(mktemp -d)
# Always clean up the temp dir, but never leak the token onto disk in any logs
trap 'rm -rf "$WORK"' EXIT

HF_BROWSER_URL="https://huggingface.co/spaces/${HF_USERNAME}/${HF_SPACE}"
HF_GIT_URL_AUTH="https://${HF_USERNAME}:${HF_TOKEN}@huggingface.co/spaces/${HF_USERNAME}/${HF_SPACE}"
HF_GIT_URL_CLEAN="https://huggingface.co/spaces/${HF_USERNAME}/${HF_SPACE}"

echo "──────────────────────────────────────────────────────────"
echo " HF Space : $HF_BROWSER_URL"
echo " Public URL on success: https://${HF_USERNAME}-${HF_SPACE}.hf.space"
echo "──────────────────────────────────────────────────────────"

echo "[1/5] cloning HF Space git into temp..."
if ! git clone -q "$HF_GIT_URL_AUTH" "$WORK/space" 2>/tmp/hf-clone.err; then
  echo "ERROR: clone failed. Make sure the Space exists at $HF_BROWSER_URL"
  echo "  Detail: $(cat /tmp/hf-clone.err 2>/dev/null | sed "s|$HF_TOKEN|<TOKEN>|g")"
  exit 1
fi
# Strip the embedded token from the remote URL so it never lands in .git/config
git -C "$WORK/space" remote set-url origin "$HF_GIT_URL_CLEAN"

echo "[2/5] syncing project files into Space..."
rsync -a --delete \
  --exclude='.git' \
  --exclude='.github' \
  --exclude='loadtest/results' \
  --exclude='*.test' \
  --exclude='node_modules' \
  --exclude='/tmp' \
  ./ "$WORK/space/"

echo "[3/5] swapping in HF-specific Dockerfile + README..."
cp deploy/hfspaces/Dockerfile "$WORK/space/Dockerfile"
cp deploy/hfspaces/README.md  "$WORK/space/README.md"
chmod +x "$WORK/space/deploy/hfspaces/start.sh"

cd "$WORK/space"
git config user.email "parthdagia05@users.noreply.github.com"
git config user.name  "Parth Dagia"

echo "[4/5] committing..."
git add -A
if git diff --cached --quiet; then
  echo "      no changes to push — Space is already up to date"
  exit 0
fi
git commit -q -m "deploy: $(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo "[5/5] pushing to HF (build will start automatically)..."
# Push using the in-URL token form so the credential never persists to .git/config.
if git push -q "$HF_GIT_URL_AUTH" main 2>&1 | sed "s|$HF_TOKEN|<TOKEN>|g"; then
  echo
  echo "──────────────────────────────────────────────────────────"
  echo " ✓ pushed. HF is now building the Space."
  echo
  echo "   Watch build:  $HF_BROWSER_URL?logs=build"
  echo "   Live URL:     https://${HF_USERNAME}-${HF_SPACE}.hf.space"
  echo "                 (give the build ~3-5 min on first push)"
  echo
  echo "   Smoke test once build is green:"
  echo "     curl https://${HF_USERNAME}-${HF_SPACE}.hf.space/health"
  echo "──────────────────────────────────────────────────────────"
else
  echo "ERROR: push failed. Check the HF Space exists and your HF_TOKEN has Write scope."
  exit 1
fi
