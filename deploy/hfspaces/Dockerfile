# syntax=docker/dockerfile:1.7
#
# HuggingFace Spaces single-container bundle for the NevUp Track 1 backend.
# Runs postgres + redis + api + worker inside one image (HF Spaces only runs
# one container, so docker-compose.yml from the main repo is unused here).
#
# Built from the repo root — paths like ./go.mod, ./migrations, etc. resolve
# against the repo content that scripts/deploy-hfspaces.sh syncs to the HF
# Space's git repo.
#
# This file is COPIED to the HF Space repo root at deploy time.

# ── Stage 1: compile both Go binaries ───────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -trimpath -o /out/api ./cmd/api \
 && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -trimpath -o /out/worker ./cmd/worker

# ── Stage 2: runtime — postgres + redis + binaries in one image ─────────────
FROM alpine:3.20

# Bundled services + minimal tooling
RUN apk add --no-cache \
    bash \
    ca-certificates \
    curl \
    postgresql16 \
    postgresql16-client \
    postgresql16-contrib \
    redis \
    su-exec \
    tzdata

# Pull binaries + assets out of the build stage
COPY --from=builder /out/api    /app/api
COPY --from=builder /out/worker /app/worker
COPY --from=builder /src/migrations               /app/migrations
COPY --from=builder /src/nevup_seed_dataset.csv   /app/nevup_seed_dataset.csv

# Startup orchestration script
COPY deploy/hfspaces/start.sh /app/start.sh

# Pre-create the runtime dirs each daemon needs and chown them.
# /var/lib/postgresql/data — Postgres cluster
# /run/postgresql          — socket dir
# /var/lib/redis           — Redis AOF + RDB
RUN mkdir -p /var/lib/postgresql/data /run/postgresql /var/lib/redis /tmp/logs \
 && chown -R postgres:postgres /var/lib/postgresql/data /run/postgresql \
 && chown -R redis:redis       /var/lib/redis \
 && chmod +x /app/start.sh

# Defaults — every value is overridable via HF Spaces' Variables UI if you
# ever want to flip them. host=localhost is correct because every service
# runs in this same container.
ENV PORT=7860 \
    DATABASE_URL="postgres://nevup@localhost:5432/nevup?sslmode=disable" \
    REDIS_URL="redis://localhost:6379/0" \
    JWT_SECRET="97791d4db2aa5f689c3cc39356ce35762f0a73aa70923039d8ef72a2840a1b02" \
    LOG_LEVEL=info \
    SEED_ON_START=true \
    SEED_FILE_PATH=/app/nevup_seed_dataset.csv \
    MIGRATIONS_DIR=/app/migrations \
    STREAM_NAME=trade-events \
    CONSUMER_GROUP=metrics-workers \
    CONSUMER_NAME=worker-1

# HF Spaces serves whatever port is declared in the README YAML's `app_port`
# field; our README sets it to 7860 to match.
EXPOSE 7860

CMD ["/app/start.sh"]
