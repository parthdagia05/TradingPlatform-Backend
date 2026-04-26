package trades

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrValidation is wrapped so handlers can distinguish "client sent garbage"
// (→ 400) from "server has a bug" (→ 500).
var ErrValidation = errors.New("validation failed")

// validate enforces every constraint the OpenAPI schema declares. We do this
// by hand instead of pulling validator/v10 here because the rules are short
// and explicit — easier to read than struct tags.
func (in *TradeInput) validate() error {
	if in.TradeID == uuid.Nil {
		return fmt.Errorf("%w: tradeId required", ErrValidation)
	}
	if in.UserID == uuid.Nil {
		return fmt.Errorf("%w: userId required", ErrValidation)
	}
	if in.SessionID == uuid.Nil {
		return fmt.Errorf("%w: sessionId required", ErrValidation)
	}
	if in.Asset == "" {
		return fmt.Errorf("%w: asset required", ErrValidation)
	}
	switch in.AssetClass {
	case AssetEquity, AssetCrypto, AssetForex:
	default:
		return fmt.Errorf("%w: assetClass must be equity|crypto|forex", ErrValidation)
	}
	switch in.Direction {
	case DirLong, DirShort:
	default:
		return fmt.Errorf("%w: direction must be long|short", ErrValidation)
	}
	if in.EntryPrice <= 0 {
		return fmt.Errorf("%w: entryPrice must be > 0", ErrValidation)
	}
	if in.Quantity <= 0 {
		return fmt.Errorf("%w: quantity must be > 0", ErrValidation)
	}
	if in.EntryAt.IsZero() {
		return fmt.Errorf("%w: entryAt required", ErrValidation)
	}
	switch in.Status {
	case StatusOpen, StatusClosed, StatusCancelled:
	default:
		return fmt.Errorf("%w: status must be open|closed|cancelled", ErrValidation)
	}
	if in.Status == StatusClosed {
		if in.ExitAt == nil || in.ExitAt.IsZero() {
			return fmt.Errorf("%w: closed trade requires exitAt", ErrValidation)
		}
		if in.ExitPrice == nil || *in.ExitPrice <= 0 {
			return fmt.Errorf("%w: closed trade requires exitPrice > 0", ErrValidation)
		}
	}
	if in.PlanAdherence != nil {
		if *in.PlanAdherence < 1 || *in.PlanAdherence > 5 {
			return fmt.Errorf("%w: planAdherence must be 1..5", ErrValidation)
		}
	}
	if in.EmotionalState != nil {
		switch *in.EmotionalState {
		case EmoCalm, EmoAnxious, EmoGreedy, EmoFearful, EmoNeutral:
		default:
			return fmt.Errorf("%w: emotionalState invalid", ErrValidation)
		}
	}
	if in.EntryRationale != nil && len(*in.EntryRationale) > 500 {
		return fmt.Errorf("%w: entryRationale must be <= 500 chars", ErrValidation)
	}
	return nil
}
