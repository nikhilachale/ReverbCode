package observer

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/scm/store"
)

type fakeProvider struct{ res ports.SCMObserveResult }

func (f fakeProvider) Provider() domain.SCMProvider { return domain.SCMProviderGitHub }
func (f fakeProvider) ObserveSessions(context.Context, ports.SCMObserveRequest, ports.SCMProviderCache) (ports.SCMObserveResult, error) {
	return f.res, nil
}

type fakeLCM struct{ facts []ports.SCMFacts }

func (f *fakeLCM) ApplySCMObservation(_ context.Context, _ domain.SessionID, facts ports.SCMFacts) error {
	f.facts = append(f.facts, facts)
	return nil
}
func (f *fakeLCM) ApplyRuntimeObservation(context.Context, domain.SessionID, ports.RuntimeFacts) error {
	return nil
}
func (f *fakeLCM) ApplyActivitySignal(context.Context, domain.SessionID, ports.ActivitySignal) error {
	return nil
}
func (f *fakeLCM) OnSpawnInitiated(context.Context, domain.SessionRecord) error { return nil }
func (f *fakeLCM) OnSpawnCompleted(context.Context, domain.SessionID, ports.SpawnOutcome) error {
	return nil
}
func (f *fakeLCM) OnKillRequested(context.Context, domain.SessionID, ports.KillReason) error {
	return nil
}
func (f *fakeLCM) TickEscalations(context.Context, time.Time) error { return nil }
func (f *fakeLCM) RunningSessions(context.Context) ([]domain.SessionRecord, error) {
	return nil, nil
}

func TestObserverPersistsSnapshotFansOutAndAppliesLCMFacts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	subj := domain.SCMSubject{SessionID: "s1", ProjectID: "p1", Provider: domain.SCMProviderGitHub, Host: "github.com", Repo: "o/r", Branch: "feat/27", PRNumber: 7}
	snap := domain.SCMSnapshot{SessionID: "s1", Subject: subj, Freshness: domain.SCMFreshnessFresh, ObservedAt: now, PR: &domain.SCMPullRequest{Number: 7, URL: "https://github.com/o/r/pull/7", State: domain.PROpen}, CI: domain.SCMCI{Summary: "failing", Checks: []domain.SCMCheck{{Name: "test", Conclusion: "failure"}}}}
	st := store.NewMemoryStore()
	lcm := &fakeLCM{}
	var events int
	o := New(st, lcm, fakeProvider{res: ports.SCMObserveResult{ProviderName: domain.SCMProviderGitHub, Subjects: []domain.SCMSubject{subj}, Snapshots: []domain.SCMSnapshot{snap}}})
	o.Clock = func() time.Time { return now }
	o.OnSnapshot = func(context.Context, domain.SCMSnapshot) error { events++; return nil }
	if err := o.Refresh(ctx, []domain.SCMSubject{subj}); err != nil {
		t.Fatal(err)
	}
	latest, ok, err := st.GetLatestSnapshot(ctx, "s1")
	if err != nil || !ok {
		t.Fatalf("latest ok=%v err=%v", ok, err)
	}
	if latest.Revision != 1 || latest.SemanticHash == "" {
		t.Fatalf("latest revision/hash %+v", latest)
	}
	if events != 1 {
		t.Fatalf("fanout events=%d", events)
	}
	if len(lcm.facts) != 1 || lcm.facts[0].CISummary != ports.CIFailing || !lcm.facts[0].Fetched {
		t.Fatalf("facts=%+v", lcm.facts)
	}
}

func TestFactsFromUnavailableSnapshotIsNotFetched(t *testing.T) {
	facts := FactsFromSnapshot(domain.SCMSnapshot{Freshness: domain.SCMFreshnessUnavailable})
	if facts.Fetched {
		t.Fatal("unavailable snapshot must project to Fetched=false")
	}
}
