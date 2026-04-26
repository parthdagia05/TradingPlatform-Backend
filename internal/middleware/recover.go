package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/nevup/trade-journal/internal/httpx"
)

// Recoverer turns any panic in a handler into a 500 with a structured JSON body
// AND a stack-trace log line tagged with the request's traceId. Without this,
// a nil-pointer deref would leave the client holding a dropped connection
// (the Go http server's default behaviour) — which the spec explicitly penalises
// ("HTTP 500 responses with an empty body will penalise your score").
func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic recovered",
						"traceId", httpx.TraceID(r.Context()),
						"panic", rec,
						"stack", string(debug.Stack()),
						"path", r.URL.Path,
						"method", r.Method,
					)
					httpx.WriteError(w, r, http.StatusInternalServerError,
						"INTERNAL_ERROR", "Unexpected error.")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
