package metrics

import (
	"context"

	"github.com/google/uuid"
)

// SessionTilt: ratio of (loss-following trades / total trades) in the session.
// "loss-following" = the previous trade in the session closed as a loss.
//
// We delegate the SQL window-function work to the repo (UpsertSessionTilt) —
// running it on every trade.closed event keeps session_metrics current.
func SessionTilt(ctx context.Context, repo *Repo, sessionID uuid.UUID) error {
	return repo.UpsertSessionTilt(ctx, sessionID)
}
