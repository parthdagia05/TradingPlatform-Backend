# syntax=docker/dockerfile:1.7

# ─────────────────────────────────────────────────────────────────────────────
# Stage 1: BUILD — compile both binaries (api + worker) inside a Go toolchain image
# ─────────────────────────────────────────────────────────────────────────────
FROM golang:1.23-alpine AS builder

# git is needed for `go mod download` of any deps hosted on git.
# ca-certificates lets HTTPS calls work during the build (e.g. proxy.golang.org).
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Copy go.mod + go.sum FIRST and download deps. This layer is cached as long
# as those two files don't change — so editing source code doesn't re-download.
COPY go.mod go.sum* ./
RUN go mod download

# Now copy the rest of the source.
COPY . .

# Build flags:
#   CGO_ENABLED=0  → produce a 100% static binary (no glibc dependency)
#                    so we can use a tiny runtime image with no C library.
#   GOOS=linux     → target Linux (the runtime image is Linux).
#   -ldflags "-s -w" → strip debug symbols and DWARF info; ~30% smaller binary.
#   -trimpath      → remove absolute paths from the binary (reproducible builds).
#
# Both binaries built in one RUN — keeps layers minimal and satisfies hadolint
# DL3059 (consecutive RUNs are wasteful: each one produces its own image layer).
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -trimpath -o /out/api ./cmd/api \
 && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -trimpath -o /out/worker ./cmd/worker

# ─────────────────────────────────────────────────────────────────────────────
# Stage 2: RUNTIME — minimal image that only contains what we need at runtime
# ─────────────────────────────────────────────────────────────────────────────
FROM alpine:3.20 AS runtime

# tzdata = correct timezone handling for ISO-8601 UTC parsing.
# ca-certificates = TLS verification for outbound HTTPS (Redis TLS, etc.).
# We do NOT install Go, git, or any dev tooling — only what runtime needs.
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S app && adduser -S app -G app

WORKDIR /app

# Copy compiled binaries from the builder stage. Stages are throwaway —
# only the files we COPY survive into the final image.
COPY --from=builder /out/api /app/api
COPY --from=builder /out/worker /app/worker

# Copy the assets the binaries read at runtime.
COPY --from=builder /src/migrations /app/migrations
COPY --from=builder /src/nevup_seed_dataset.csv /app/nevup_seed_dataset.csv

# Run as a non-root user. If the container is ever compromised, the attacker
# doesn't immediately own the host file system.
USER app

EXPOSE 8080

# Default command = the API server. docker-compose overrides this for the
# worker service (command: ["/app/worker"]).
ENTRYPOINT ["/app/api"]
