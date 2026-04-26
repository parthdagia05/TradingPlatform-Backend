package metrics

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/nevup/trade-journal/internal/httpx"
)

// Handler exposes GET /users/{userId}/metrics. Tenancy enforcement happens
// in middleware (RequireUserMatch) before this handler runs.
type Handler struct {
	repo *Repo
}

func NewHandler(repo *Repo) *Handler { return &Handler{repo: repo} }

func (h *Handler) Mount(r chi.Router) {
	r.Get("/users/{userId}/metrics", h.getMetrics)
}

// MetricsResponse matches the OpenAPI BehavioralMetrics schema.
type MetricsResponse struct {
	UserID                  uuid.UUID                `json:"userId"`
	Granularity             Granularity              `json:"granularity"`
	From                    time.Time                `json:"from"`
	To                      time.Time                `json:"to"`
	PlanAdherenceScore      *float64                 `json:"planAdherenceScore,omitempty"`
	SessionTiltIndex        *float64                 `json:"sessionTiltIndex,omitempty"`
	WinRateByEmotionalState map[string]EmotionStats  `json:"winRateByEmotionalState"`
	RevengeTrades           int                      `json:"revengeTrades"`
	OvertradingEvents       int                      `json:"overtradingEvents"`
	Timeseries              []Bucket                 `json:"timeseries"`
}

func (h *Handler) getMetrics(w http.ResponseWriter, r *http.Request) {
	log := httpx.Logger(r.Context())

	userID, err := uuid.Parse(chi.URLParam(r, "userId"))
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest,
			"BAD_REQUEST", "userId path parameter must be a UUID.")
		return
	}

	q := r.URL.Query()
	from, errFrom := parseRFC3339(q.Get("from"))
	to, errTo := parseRFC3339(q.Get("to"))
	if errFrom != nil || errTo != nil {
		httpx.WriteError(w, r, http.StatusBadRequest,
			"BAD_REQUEST", "from/to must be ISO-8601 / RFC3339.")
		return
	}
	if !from.Before(to) {
		httpx.WriteError(w, r, http.StatusBadRequest,
			"BAD_REQUEST", "from must be earlier than to.")
		return
	}

	gran, err := parseGranularity(q.Get("granularity"))
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest,
			"BAD_REQUEST", err.Error())
		return
	}

	snap, err := h.repo.LoadSnapshot(r.Context(), userID)
	if err != nil {
		log.Error("load snapshot", "err", err)
		httpx.WriteError(w, r, http.StatusInternalServerError,
			"INTERNAL_ERROR", "Could not load metrics.")
		return
	}

	buckets, err := h.repo.LoadBuckets(r.Context(), userID, from, to, gran)
	if err != nil {
		log.Error("load buckets", "err", err)
		httpx.WriteError(w, r, http.StatusInternalServerError,
			"INTERNAL_ERROR", "Could not load timeseries.")
		return
	}

	resp := MetricsResponse{
		UserID:                  userID,
		Granularity:             gran,
		From:                    from,
		To:                      to,
		PlanAdherenceScore:      snap.PlanAdherenceScore,
		SessionTiltIndex:        snap.SessionTiltIndex,
		WinRateByEmotionalState: snap.WinRateByEmotion,
		RevengeTrades:           snap.RevengeTrades,
		OvertradingEvents:       snap.OvertradingEvents,
		Timeseries:              buckets,
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func parseGranularity(s string) (Granularity, error) {
	switch Granularity(s) {
	case GranHourly, GranDaily, GranRolling30:
		return Granularity(s), nil
	case "":
		return "", errors.New("granularity is required (hourly|daily|rolling30d)")
	default:
		return "", fmt.Errorf("granularity %q invalid; want hourly|daily|rolling30d", s)
	}
}

func parseRFC3339(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("empty timestamp")
	}
	return time.Parse(time.RFC3339Nano, s)
}
