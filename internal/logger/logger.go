// Package logger configures the application's structured logger.
//
// We use Go's stdlib log/slog (introduced in 1.21). It emits structured JSON
// without an external dependency - exactly what the hackathon spec requires:
//
//   { "time": "...", "level": "INFO", "msg": "...",
//     "traceId": "...", "userId": "...", "latency": 142, "statusCode": 200 }
//
// One *slog.Logger instance is built at startup and passed down to every
// component that needs it. We do NOT use a global logger - that hides
// dependencies and makes tests harder.
package logger

import (
	"log/slog"
	"os"
	"strings"
)

// New returns a JSON-emitting logger writing to stdout at the given level.
//
// "debug" | "info" | "warn" | "error" - case-insensitive. Anything else
// silently falls back to "info" (we'd rather log too much than crash on a typo).
//
// Stdout (not stderr, not a file) is the right destination for a containerised
// app: Docker captures stdout into `docker logs`, and platforms like
// Cloud Run / ECS forward it to their log aggregators automatically.
func New(level string) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level: parseLevel(level),

		// AddSource: false - we keep the JSON small. Stack traces come from
		// our error responses (with traceId), not from individual log lines.
		AddSource: false,
	}

	handler := slog.NewJSONHandler(os.Stdout, opts)
	return slog.New(handler)
}

// parseLevel maps a string name to slog's numeric level.
// Defaults to Info on any unknown value - silent degrade, never panic on misconfig.
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
