package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	scmstore "github.com/aoagents/agent-orchestrator/backend/internal/scm/store"
)

func TestConsumerAppliesLatestSnapshotOnce(t *testing.T) {
	ctx := context.Background()
	store := scmstore.NewMemoryStore()
	subj := domain.SCMSubject{
		SessionID: "mer-1", ProjectID: "mer", Provider: domain.SCMProviderGitHub,
		Host: "github.com", Repo: "aoagents/agent-orchestrator", Branch: "feat/27", PRNumber: 28,
		PRURL: "https://github.com/aoagents/agent-orchestrator/pull/28",
	}
	snap, changed, err := store.SaveSnapshot(ctx, domain.SCMSnapshot{
		SessionID:  subj.SessionID,
		Subject:    subj,
		Freshness:  domain.SCMFreshnessFresh,
		ObservedAt: time.Now(),
		PR: &domain.SCMPullRequest{
			Number: subj.PRNumber, URL: subj.PRURL, State: domain.PRDraft, Draft: true,
		},
		CI: domain.SCMCI{Summary: "failing"},
	})
	if err != nil || !changed {
		t.Fatalf("save snapshot changed=%v err=%v", changed, err)
	}
	lcm := &captureLCM{}
	consumer := NewConsumer(store, lcm, nil)
	payload, _ := json.Marshal(map[string]any{"sessionId": string(subj.SessionID), "revision": snap.Revision})
	event := cdc.Event{Seq: 1, ProjectID: "mer", SessionID: string(subj.SessionID), Type: cdc.EventSCMSnapshotCreated, Payload: payload}
	if err := consumer.Handle(ctx, event); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Handle(ctx, event); err != nil {
		t.Fatal(err)
	}
	if lcm.calls != 1 {
		t.Fatalf("consumer should apply event once, got %d calls", lcm.calls)
	}
	if !lcm.last.Fetched || lcm.last.PRState != domain.PRDraft || !lcm.last.Draft || lcm.last.CISummary != ports.CIFailing {
		t.Fatalf("facts not projected correctly: %+v", lcm.last)
	}
}

type captureLCM struct {
	calls int
	last  ports.SCMFacts
}

func (c *captureLCM) ApplySCMObservation(_ context.Context, _ domain.SessionID, f ports.SCMFacts) error {
	c.calls++
	c.last = f
	return nil
}
func (c *captureLCM) ApplyRuntimeObservation(context.Context, domain.SessionID, ports.RuntimeFacts) error {
	return nil
}
func (c *captureLCM) ApplyActivitySignal(context.Context, domain.SessionID, ports.ActivitySignal) error {
	return nil
}
func (c *captureLCM) ApplyPRObservation(context.Context, domain.SessionID, ports.PRObservation) error {
	return nil
}
func (c *captureLCM) OnSpawnCompleted(context.Context, domain.SessionID, ports.SpawnOutcome) error {
	return nil
}
func (c *captureLCM) OnKillRequested(context.Context, domain.SessionID, domain.TerminationReason) error {
	return nil
}
func (c *captureLCM) TickEscalations(context.Context, time.Time) error { return nil }
func (c *captureLCM) RunningSessions(context.Context) ([]domain.SessionRecord, error) {
	return nil, nil
}
