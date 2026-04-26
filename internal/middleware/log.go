package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/nevup/trade-journal/internal/httpx"
)

// statusRecorder wraps http.ResponseWriter so we can read back the status code
// after the handler returns. The stdlib doesn't expose this; we have to capture.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK // implicit when handler skips WriteHeader
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Logger emits one structured JSON line per request after the handler runs.
// Fields match the spec exactly: traceId, userId, latency, statusCode, plus
// method/path for debugging. It also injects a per-request *slog.Logger into
// the context so handlers can log with the same fields auto-attached.
func Logger(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}

			// Attach a logger pre-bound with the traceId so handler logs are
			// automatically correlated. userId is added later by the auth
			// middleware (it doesn't exist yet for unauthenticated routes).
			reqLog := base.With("traceId", httpx.TraceID(r.Context()))
			ctx := httpx.WithLogger(r.Context(), reqLog)

			next.ServeHTTP(rec, r.WithContext(ctx))

			latency := time.Since(start).Milliseconds()
			fields := []any{
				"traceId", httpx.TraceID(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"statusCode", rec.status,
				"latency", latency,
				"bytes", rec.bytes,
			}
			if uid, ok := httpx.UserID(r.Context()); ok {
				fields = append(fields, "userId", uid.String())
			}
			// 5xx  Error, 4xx  Warn, else Info. Lets log filters surface
			// problems without grepping every line.
			switch {
			case rec.status >= 500:
				base.Error("request", fields...)
			case rec.status >= 400:
				base.Warn("request", fields...)
			default:
				base.Info("request", fields...)
			}
		})
	}
}
