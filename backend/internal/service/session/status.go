package session

import (
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// noSignalGrace is how long after spawn/restore a session may stay silent
// before its idle reading is downgraded to StatusNoSignal. It covers the
// agent's TUI boot plus the gap to its first hook callback (Codex fires
// SessionStart/UserPromptSubmit on the first turn, seconds after an
// auto-submitted prompt); past it, a session that has never signaled is
// indistinguishable from one with a broken hook pipeline, and the dashboard
// must not claim a confident "idle".
const noSignalGrace = 90 * time.Second

func deriveStatus(rec domain.SessionRecord, pr *domain.PRFacts, now time.Time) domain.SessionStatus {
	if rec.IsTerminated {
		if pr != nil && pr.Merged {
			return domain.StatusMerged
		}
		return domain.StatusTerminated
	}

	if rec.Activity.State == domain.ActivityWaitingInput {
		return domain.StatusNeedsInput
	}

	if pr != nil {
		if pr.Merged {
			return domain.StatusMerged
		}
		if !pr.Closed {
			return prPipelineStatus(*pr)
		}
	}

	if rec.Activity.State == domain.ActivityActive {
		return domain.StatusWorking
	}

	// No hook callback has ever arrived for this spawn/restore. The seeded
	// LastActivityAt marks the launch, so once the grace passes the honest
	// status is "no signal", not "idle".
	if rec.FirstSignalAt.IsZero() && now.Sub(rec.Activity.LastActivityAt) > noSignalGrace {
		return domain.StatusNoSignal
	}
	return domain.StatusIdle
}

func prPipelineStatus(pr domain.PRFacts) domain.SessionStatus {
	switch {
	case pr.CI == domain.CIFailing:
		return domain.StatusCIFailed
	case pr.Draft:
		return domain.StatusDraft
	case pr.Review == domain.ReviewChangesRequest || pr.ReviewComments:
		return domain.StatusChangesRequested
	case pr.Mergeability == domain.MergeMergeable:
		return domain.StatusMergeable
	case pr.Review == domain.ReviewApproved:
		return domain.StatusApproved
	case pr.Review == domain.ReviewRequired:
		return domain.StatusReviewPending
	default:
		return domain.StatusPROpen
	}
}
