package readmodel

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// AttachLatestSCM enriches the session read-model with the latest normalized
// SCM snapshot. Dashboard/API code should consume these typed fields rather
// than legacy metadata blobs such as prEnrichment or prReviewComments.
func AttachLatestSCM(ctx context.Context, store ports.SCMStore, sessions []domain.Session) ([]domain.Session, error) {
	out := make([]domain.Session, len(sessions))
	copy(out, sessions)
	if store == nil {
		return out, nil
	}
	for i := range out {
		snap, ok, err := store.GetLatestSnapshot(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		if ok {
			out[i].SCM = &snap
		}
	}
	return out, nil
}
