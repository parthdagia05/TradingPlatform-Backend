// Package trades implements the canonical trade resource: write path,
// idempotency, repo, and handler. POST /trades + GET /trades/{tradeId} live here.
package trades

import (
	"time"

	"github.com/google/uuid"
)

// AssetClass / Direction / Status / EmotionalState / Outcome are string-typed
// enums matching the OpenAPI spec exactly. We validate against these in
// validate() before touching the DB, which short-circuits a round-trip on
// obviously malformed input.
type AssetClass string
type Direction string
type Status string
type EmotionalState string
type Outcome string

const (
	AssetEquity AssetClass = "equity"
	AssetCrypto AssetClass = "crypto"
	AssetForex  AssetClass = "forex"
)

const (
	DirLong  Direction = "long"
	DirShort Direction = "short"
)

const (
	StatusOpen      Status = "open"
	StatusClosed    Status = "closed"
	StatusCancelled Status = "cancelled"
)

const (
	EmoCalm    EmotionalState = "calm"
	EmoAnxious EmotionalState = "anxious"
	EmoGreedy  EmotionalState = "greedy"
	EmoFearful EmotionalState = "fearful"
	EmoNeutral EmotionalState = "neutral"
)

const (
	OutcomeWin  Outcome = "win"
	OutcomeLoss Outcome = "loss"
)

// TradeInput is the POST /trades body. Mirrors the OpenAPI TradeInput schema.
// Pointers are used for nullable fields so the JSON decoder distinguishes
// "absent" (nil) from "explicit null" (still nil with omitempty) - pgx then
// writes SQL NULL.
type TradeInput struct {
	TradeID         uuid.UUID       `json:"tradeId"`
	UserID          uuid.UUID       `json:"userId"`
	SessionID       uuid.UUID       `json:"sessionId"`
	Asset           string          `json:"asset"`
	AssetClass      AssetClass      `json:"assetClass"`
	Direction       Direction       `json:"direction"`
	EntryPrice      float64         `json:"entryPrice"`
	ExitPrice       *float64        `json:"exitPrice,omitempty"`
	Quantity        float64         `json:"quantity"`
	EntryAt         time.Time       `json:"entryAt"`
	ExitAt          *time.Time      `json:"exitAt,omitempty"`
	Status          Status          `json:"status"`
	PlanAdherence   *int            `json:"planAdherence,omitempty"`
	EmotionalState  *EmotionalState `json:"emotionalState,omitempty"`
	EntryRationale  *string         `json:"entryRationale,omitempty"`
}

// Trade is the response shape - input fields plus computed/server-side ones.
type Trade struct {
	TradeInput
	Outcome     *Outcome  `json:"outcome,omitempty"`
	PnL         *float64  `json:"pnl,omitempty"`
	RevengeFlag bool      `json:"revengeFlag"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}
