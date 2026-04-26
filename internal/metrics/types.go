// Package metrics implements the 5 behavioural metrics from the spec:
//
//  1. Plan Adherence Score   - rolling 10-trade avg of planAdherence
//  2. Revenge Trade Flag     - opens within 90s of a losing close + anxious/fearful
//  3. Session Tilt Index     - loss-following / total trades in the session
//  4. Win Rate by Emotion    - running per-emotion win/loss counts
//  5. Overtrading Detector   - > 10 trades in any 30-minute sliding window
//
// Each calculator is invoked by the async worker on the relevant event
// (trade.opened or trade.closed) and persists its output via repo.go.
package metrics

import (
	"time"

	"github.com/google/uuid"
)

// UserMetrics is the snapshot returned for a user.
type UserMetrics struct {
	UserID                 uuid.UUID                 `json:"userId"`
	PlanAdherenceScore     *float64                  `json:"planAdherenceScore,omitempty"`
	SessionTiltIndex       *float64                  `json:"sessionTiltIndex,omitempty"`
	WinRateByEmotion       map[string]EmotionStats   `json:"winRateByEmotionalState"`
	RevengeTrades          int                       `json:"revengeTrades"`
	OvertradingEvents      int                       `json:"overtradingEvents"`
}

type EmotionStats struct {
	Wins    int     `json:"wins"`
	Losses  int     `json:"losses"`
	WinRate float64 `json:"winRate"`
}

// Bucket is one row in the metrics timeseries response.
type Bucket struct {
	BucketAt         time.Time `json:"bucket"`
	TradeCount       int       `json:"tradeCount"`
	WinRate          float64   `json:"winRate"`
	PnL              float64   `json:"pnl"`
	AvgPlanAdherence float64   `json:"avgPlanAdherence"`
}

// Granularity matches the spec's enum.
type Granularity string

const (
	GranHourly    Granularity = "hourly"
	GranDaily     Granularity = "daily"
	GranRolling30 Granularity = "rolling30d"
)
