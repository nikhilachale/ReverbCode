package main

import (
	"context"
	"log/slog"

	ghscm "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/github"
	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/poller"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/scm/command"
	scmevents "github.com/aoagents/agent-orchestrator/backend/internal/scm/events"
	"github.com/aoagents/agent-orchestrator/backend/internal/scm/observer"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type scmStack struct {
	Observer ports.SCMObserver
	Commands *command.Service
	registry *poller.Registry
	unsub    func()
}

func startSCM(ctx context.Context, store *sqlite.Store, lcm ports.LifecycleManager, cdcPipe *cdcPipeline, logger *slog.Logger) (*scmStack, error) {
	provider := ghscm.NewProvider(ghscm.ProviderOptions{Logger: logger})
	obs := observer.New(store, nil, provider)
	obs.Logger = logger

	consumer := scmevents.NewConsumer(store, lcm, logger)
	unsub := func() {}
	if cdcPipe != nil && cdcPipe.Broadcaster != nil {
		unsub = cdcPipe.Broadcaster.Subscribe(func(e cdc.Event) {
			if e.Type != cdc.EventSCMSnapshotCreated {
				return
			}
			go func() {
				if err := consumer.Handle(ctx, e); err != nil && logger != nil {
					logger.Error("scm event consumer: apply failed", "seq", e.Seq, "session", e.SessionID, "err", err)
				}
			}()
		})
	}

	sched := &observer.Scheduler{
		Observer: obs,
		Store:    store,
		Projects: scmProjectSource{store: store},
		Sessions: scmSessionSource{store: store},
		PRs:      scmPRSource{store: store},
		Logger:   logger,
	}
	reg := poller.NewRegistry(logger)
	if err := reg.Register(sched); err != nil {
		unsub()
		return nil, err
	}
	reg.Start(ctx)

	cmds := command.New(store, obs, provider)
	cmds.Logger = logger

	return &scmStack{Observer: obs, Commands: cmds, registry: reg, unsub: unsub}, nil
}

func (s *scmStack) Stop() {
	if s == nil {
		return
	}
	if s.unsub != nil {
		s.unsub()
	}
	if s.registry != nil {
		s.registry.Stop()
	}
}

type scmProjectSource struct{ store *sqlite.Store }

func (s scmProjectSource) ListSCMProjects(ctx context.Context) ([]observer.ProjectConfig, error) {
	rows, err := s.store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]observer.ProjectConfig, 0, len(rows))
	for _, r := range rows {
		out = append(out, observer.ProjectConfig{
			ID:            domain.ProjectID(r.ID),
			Path:          r.Path,
			RepoOriginURL: r.RepoOriginURL,
			DefaultBranch: "main",
		})
	}
	return out, nil
}

type scmSessionSource struct{ store *sqlite.Store }

func (s scmSessionSource) ListSCMSessions(ctx context.Context) ([]domain.SessionRecord, error) {
	return s.store.ListAllSessions(ctx)
}

type scmPRSource struct{ store *sqlite.Store }

func (s scmPRSource) LatestPRBinding(ctx context.Context, sessionID domain.SessionID) (observer.PRBinding, bool, error) {
	rows, err := s.store.ListPRsBySession(ctx, string(sessionID))
	if err != nil {
		return observer.PRBinding{}, false, err
	}
	if len(rows) == 0 {
		return observer.PRBinding{}, false, nil
	}
	pick := rows[0]
	for _, r := range rows {
		if r.State == "draft" || r.State == "open" {
			pick = r
			break
		}
	}
	return observer.PRBinding{Number: int(pick.Number), URL: pick.URL}, true, nil
}
