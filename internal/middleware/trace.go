// Package middleware contains the per-request HTTP middleware chain:
// trace-id  request logger  panic recoverer  JWT auth.
package middleware

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/nevup/trade-journal/internal/httpx"
)

// HeaderTraceID is the canonical header name for the per-request id we generate
// (or accept from the client if they sent one). It's exposed in the response
// so callers can correlate their request with our structured logs.
const HeaderTraceID = "X-Trace-Id"

// Trace ensures every request has a traceId in its context AND on the response.
// If the caller sent X-Trace-Id we honour it; otherwise we generate a UUIDv4.
// The traceId flows into every log line and every error body for that request.
func Trace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderTraceID)
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set(HeaderTraceID, id)
		ctx := httpx.WithTraceID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
