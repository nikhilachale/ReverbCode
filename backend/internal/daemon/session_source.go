package daemon

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/terminal"
)

// daemonSessionSource implements terminal.SessionSource over the sqlite store.
// It resolves each session's derived display Status (record + PR facts) so the
// terminal manager can build SessionPatch frames without importing the service layer.
type daemonSessionSource struct {
	store *sqlite.Store
}

var _ terminal.SessionSource = (*daemonSessionSource)(nil)

func (s *daemonSessionSource) AllSessions(ctx context.Context) ([]domain.Session, error) {
	recs, err := s.store.ListAllSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Session, 0, len(recs))
	for _, rec := range recs {
		out = append(out, s.toSession(ctx, rec))
	}
	return out, nil
}

func (s *daemonSessionSource) toSession(ctx context.Context, rec domain.SessionRecord) domain.Session {
	pr, ok, _ := s.store.GetDisplayPRFactsForSession(ctx, rec.ID)
	if ok {
		return domain.Session{SessionRecord: rec, Status: domain.DeriveStatus(rec, &pr)}
	}
	return domain.Session{SessionRecord: rec, Status: domain.DeriveStatus(rec, nil)}
}
