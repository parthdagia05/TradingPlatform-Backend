#!/usr/bin/env bash
# start.sh - HuggingFace Spaces entrypoint.
#
# Boot order (matches docker-compose's healthcheck-gated graph):
#   1. Initialize Postgres data dir if first boot.
#   2. Start Postgres in the background, wait for it to accept connections.
#   3. Create role nevup + database nevup if missing.
#   4. Start Redis in the background, wait for PING.
#   5. Start the worker in the background.
#   6. Exec the API in the foreground (PID 1 - keeps the container alive).
#
# All four processes share the container. They talk to each other on
# 127.0.0.1, so the env defaults (DATABASE_URL=...@localhost, REDIS_URL=...
# @localhost) need no override.

set -euo pipefail

PGDATA="${PGDATA:-/var/lib/postgresql/data}"
PGUSER_OS="postgres"
PGLOG="/tmp/logs/postgres.log"
WORKERLOG="/tmp/logs/worker.log"
mkdir -p /tmp/logs

# ── 1. initdb (idempotent) ──────────────────────────────────────────────────
if [ ! -s "$PGDATA/PG_VERSION" ]; then
  echo "[init] postgres data dir is empty - running initdb"
  su-exec "$PGUSER_OS" initdb -D "$PGDATA" --auth=trust --encoding=UTF8 -A trust >/tmp/logs/initdb.log 2>&1
  # Listen on localhost only - safer + matches in-container topology
  echo "listen_addresses = 'localhost'"        >> "$PGDATA/postgresql.conf"
  # Trust auth for loopback (no passwords needed inside one container)
  cat >>"$PGDATA/pg_hba.conf" <<EOF
host  all  all  127.0.0.1/32  trust
host  all  all  ::1/128       trust
EOF
fi

# ── 2. start postgres + wait ────────────────────────────────────────────────
echo "[start] postgres..."
su-exec "$PGUSER_OS" pg_ctl -D "$PGDATA" -l "$PGLOG" -w -t 30 start
for i in $(seq 1 30); do
  if su-exec "$PGUSER_OS" pg_isready -h localhost -q; then break; fi
  sleep 0.5
done

# ── 3. ensure role + db ────────────────────────────────────────────────────
exists_role=$(su-exec "$PGUSER_OS" psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='nevup'" || true)
if [ "$exists_role" != "1" ]; then
  echo "[init] creating role nevup"
  su-exec "$PGUSER_OS" psql -c "CREATE ROLE nevup WITH LOGIN SUPERUSER;"
fi

exists_db=$(su-exec "$PGUSER_OS" psql -tAc "SELECT 1 FROM pg_database WHERE datname='nevup'" || true)
if [ "$exists_db" != "1" ]; then
  echo "[init] creating database nevup"
  su-exec "$PGUSER_OS" psql -c "CREATE DATABASE nevup OWNER nevup;"
fi

# ── 4. start redis + wait ──────────────────────────────────────────────────
echo "[start] redis..."
redis-server --daemonize yes \
             --bind 127.0.0.1 \
             --appendonly yes \
             --dir /var/lib/redis \
             --logfile /tmp/logs/redis.log
for i in $(seq 1 20); do
  if redis-cli -h 127.0.0.1 ping >/dev/null 2>&1; then break; fi
  sleep 0.2
done

# ── 5. start worker in background ──────────────────────────────────────────
echo "[start] worker..."
/app/worker > "$WORKERLOG" 2>&1 &

# ── 6. start api in foreground (PID becomes container's main process) ──────
echo "[start] api on :${PORT:-7860}"
exec /app/api
