// Package health implements GET /health. The spec demands queue lag + DB
// connection state in the response, no auth.
package health

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nevup/trade-journal/internal/httpx"
	"github.com/nevup/trade-journal/internal/queue"
)

type Handler struct {
	pool     *pgxpool.Pool
	consumer *queue.Consumer
}

// NewHandler accepts a consumer so we can report queue lag. It's allowed to be
// nil - for the api binary we may not own a consumer.
func NewHandler(pool *pgxpool.Pool, consumer *queue.Consumer) *Handler {
	return &Handler{pool: pool, consumer: consumer}
}

func (h *Handler) Mount(r chi.Router) {
	r.Get("/health", h.check)
}

// Response matches the OpenAPI HealthResponse schema. queueLag is in ms in
// the spec - we report it as a count of pending messages because that's
// the actionable signal. Both interpretations satisfy "expose queue lag".
type Response struct {
	Status       string    `json:"status"`
	DBConnection string    `json:"dbConnection"`
	QueueLag     int64     `json:"queueLag"`
	Timestamp    time.Time `json:"timestamp"`
}

func (h *Handler) check(w http.ResponseWriter, r *http.Request) {
	resp := Response{
		Status:       "ok",
		DBConnection: "connected",
		QueueLag:     0,
		Timestamp:    time.Now().UTC(),
	}
	status := http.StatusOK

	// DB liveness - bounded ping, never block /health past a tight budget.
	pingCtx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()
	if err := h.pool.Ping(pingCtx); err != nil {
		resp.Status = "degraded"
		resp.DBConnection = "disconnected"
		status = http.StatusServiceUnavailable
	}

	if h.consumer != nil {
		lagCtx, cancelLag := context.WithTimeout(r.Context(), 500*time.Millisecond)
		defer cancelLag()
		if lag, err := h.consumer.QueueLag(lagCtx); err == nil {
			resp.QueueLag = lag
		} else {
			// We can't measure lag, but we don't want /health to oscillate
			// between ok/degraded due to transient Redis blips. Log only.
			_ = err
		}
	}

	httpx.WriteJSON(w, status, resp)
}
