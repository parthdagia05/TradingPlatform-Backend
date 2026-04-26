package trades

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/nevup/trade-journal/internal/httpx"
	"github.com/nevup/trade-journal/internal/queue"
)

// Handler holds the deps the trade endpoints need. Keeps the routes thin.
type Handler struct {
	repo     *Repo
	producer *queue.Producer
}

func NewHandler(repo *Repo, p *queue.Producer) *Handler {
	return &Handler{repo: repo, producer: p}
}

// Mount wires the routes onto the router. The auth middleware is applied
// at the parent router level - here we just declare paths.
func (h *Handler) Mount(r chi.Router) {
	r.Post("/trades", h.create)
	r.Get("/trades/{tradeId}", h.get)
}

// POST /trades
//   - 200 on success OR idempotent re-submit
//   - 400 on validation error
//   - 403 if jwt.sub != body.userId (cross-tenant write attempt)
//   - 401 handled upstream by Authenticator
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	log := httpx.Logger(r.Context())

	var in TradeInput
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest,
			"BAD_REQUEST", "Malformed JSON body: "+err.Error())
		return
	}

	if err := in.validate(); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}

	// Cross-tenant write check: caller can only create trades for themselves.
	tokenUID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, r, http.StatusUnauthorized,
			"UNAUTHORIZED", "Missing authenticated user.")
		return
	}
	if tokenUID != in.UserID {
		httpx.WriteError(w, r, http.StatusForbidden,
			"FORBIDDEN", "Cross-tenant write denied.")
		return
	}

	t, inserted, err := h.repo.Insert(r.Context(), &in)
	if err != nil {
		log.Error("trade insert failed", "err", err)
		httpx.WriteError(w, r, http.StatusInternalServerError,
			"INTERNAL_ERROR", "Could not persist trade.")
		return
	}

	// Fire-and-forget the metrics events on a fresh, time-bounded context so
	// a slow Redis can't drag out the request past our p95 budget. Spec says
	// the async pipeline must not block the write path - this enforces it.
	if h.producer != nil && inserted {
		go func(ev queue.Event) {
			pubCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			if _, err := h.producer.Publish(pubCtx, ev); err != nil {
				log.Warn("publish event dropped",
					"type", ev.Type, "tradeId", ev.TradeID, "err", err)
			}
		}(eventFor(t))
	}

	// 200 in both branches - idempotent contract.
	httpx.WriteJSON(w, http.StatusOK, t)
}

// GET /trades/{tradeId}
//   - 200 with the trade
//   - 403 if the trade belongs to someone else
//   - 404 only if the id doesn't exist (and the caller is also not blocked
//     on tenancy - we check tenancy first to never leak existence)
func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	log := httpx.Logger(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "tradeId"))
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest,
			"BAD_REQUEST", "tradeId path parameter must be a UUID.")
		return
	}
	tokenUID, _ := httpx.UserID(r.Context())

	t, err := h.repo.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.WriteError(w, r, http.StatusNotFound,
				"TRADE_NOT_FOUND", "Trade with the given tradeId does not exist.")
			return
		}
		log.Error("trade get failed", "err", err, "tradeId", id)
		httpx.WriteError(w, r, http.StatusInternalServerError,
			"INTERNAL_ERROR", "Could not load trade.")
		return
	}

	// Tenancy check AFTER load: returning 403 is correct per spec
	// ("Any mismatch must return HTTP 403 - never 404.").
	if t.UserID != tokenUID {
		httpx.WriteError(w, r, http.StatusForbidden,
			"FORBIDDEN", "Cross-tenant access denied.")
		return
	}

	httpx.WriteJSON(w, http.StatusOK, t)
}

// eventFor maps a persisted trade into the queue.Event we publish.
// Closed trades produce trade.closed; open trades produce trade.opened.
func eventFor(t *Trade) queue.Event {
	typ := queue.EventTradeOpened
	at := t.EntryAt
	if t.Status == StatusClosed && t.ExitAt != nil {
		typ = queue.EventTradeClosed
		at = *t.ExitAt
	}
	return queue.Event{
		Type:       typ,
		TradeID:    t.TradeID.String(),
		UserID:     t.UserID.String(),
		SessionID:  t.SessionID.String(),
		OccurredAt: at,
	}
}
