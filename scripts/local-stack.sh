#!/usr/bin/env bash
# local-stack.sh — manage a user-owned Postgres for the no-Docker dev stack.
#
# Usage:
#   ./scripts/local-stack.sh up        # initdb + start PG on /tmp:5433
#   ./scripts/local-stack.sh down      # stop PG and remove the cluster dir
#   ./scripts/local-stack.sh status    # is it running?
#   ./scripts/local-stack.sh psql      # open a psql shell
#
# The cluster lives at /tmp/nevup-pg, listens on a unix socket at /tmp:5433,
# and uses trust auth — no passwords. /tmp is wiped on reboot, which is fine
# for dev: `up` does a fresh initdb every time the data dir is missing.

set -euo pipefail

PG_BIN="${PG_BIN:-/usr/lib/postgresql/16/bin}"
PGDATA="${PGDATA:-/tmp/nevup-pg}"
PGPORT="${PGPORT:-5433}"
PGSOCK="${PGSOCK:-/tmp}"
PGLOG="${PGLOG:-/tmp/nevup-pg.log}"

cmd="${1:-up}"

case "$cmd" in
  up)
    if [[ ! -s "$PGDATA/PG_VERSION" ]]; then
      mkdir -p "$PGDATA"
      "$PG_BIN/initdb" -D "$PGDATA" -U nevup --auth=trust --encoding=UTF8 -A trust >/dev/null
      echo "initdb ok: $PGDATA"
    fi
    if "$PG_BIN/pg_isready" -h "$PGSOCK" -p "$PGPORT" >/dev/null 2>&1; then
      echo "already running"
    else
      "$PG_BIN/pg_ctl" -D "$PGDATA" -o "-p $PGPORT -k $PGSOCK" -l "$PGLOG" start >/dev/null
      sleep 1
    fi
    "$PG_BIN/psql" -h "$PGSOCK" -p "$PGPORT" -U nevup -d postgres -tAc \
      "SELECT 1 FROM pg_database WHERE datname='nevup'" 2>/dev/null \
      | grep -q 1 || \
      "$PG_BIN/psql" -h "$PGSOCK" -p "$PGPORT" -U nevup -d postgres -c 'CREATE DATABASE nevup;' >/dev/null
    echo
    echo "Postgres ready at:"
    echo "  socket: $PGSOCK:$PGPORT"
    echo "  DSN:    postgres://nevup@/nevup?host=$PGSOCK&port=$PGPORT&sslmode=disable"
    ;;
  down)
    if "$PG_BIN/pg_isready" -h "$PGSOCK" -p "$PGPORT" >/dev/null 2>&1; then
      "$PG_BIN/pg_ctl" -D "$PGDATA" stop -m fast >/dev/null
    fi
    rm -rf "$PGDATA" "$PGLOG"
    echo "stopped and cleaned up"
    ;;
  status)
    "$PG_BIN/pg_isready" -h "$PGSOCK" -p "$PGPORT"
    ;;
  psql)
    exec "$PG_BIN/psql" -h "$PGSOCK" -p "$PGPORT" -U nevup -d nevup
    ;;
  *)
    echo "usage: $0 {up|down|status|psql}" >&2
    exit 2
    ;;
esac
