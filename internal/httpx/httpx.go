// Package httpx holds shared HTTP plumbing: the canonical error response
// envelope, the request-scoped context keys, and tiny JSON helpers.
//
// We keep these in their own package (not inside middleware/) because both
// middleware AND handlers need them - and importing handler code from
// middleware would create a cycle.
package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

// Context keys
// Using a typed key prevents accidental collisions with other packages that
// store things in context.Context - best practice from the Go std library.

type ctxKey int

const (
	keyTraceID ctxKey = iota
	keyUserID
	keyLogger
)

func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyTraceID, id)
}
func TraceID(ctx context.Context) string {
	v, _ := ctx.Value(keyTraceID).(string)
	return v
}

func WithUserID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, keyUserID, id)
}
func UserID(ctx context.Context) (uuid.UUID, bool) {
	v, ok := ctx.Value(keyUserID).(uuid.UUID)
	return v, ok
}

func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, keyLogger, log)
}
func Logger(ctx context.Context) *slog.Logger {
	if v, ok := ctx.Value(keyLogger).(*slog.Logger); ok {
		return v
	}
	return slog.Default()
}

// Error response envelope (matches OpenAPI ErrorResponse schema)

// ErrorResponse is the JSON body of every 4xx/5xx response. Keeping the shape
// uniform makes Track 3's frontend simpler and matches the spec's contract.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	TraceID string `json:"traceId"`
}

// WriteError serializes an error response and writes it with the given status.
// The traceId is pulled from the context so it matches the structured log line
// for the same request - exactly what the spec demands.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{
		Error:   code,
		Message: msg,
		TraceID: TraceID(r.Context()),
	})
}

// WriteJSON serializes v with the given status code. Centralised so we never
// forget the Content-Type header.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
