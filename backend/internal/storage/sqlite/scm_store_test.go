package sqlite

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func seedSCMSession(t *testing.T, s *Store) domain.SessionRecord {
	t.Helper()
	seedProject(t, s, "mer")
	rec, err := s.CreateSession(context.Background(), sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func sampleSCMSubject(sessionID domain.SessionID) domain.SCMSubject {
	return domain.SCMSubject{
		SessionID:      sessionID,
		ProjectID:      "mer",
		Provider:       domain.SCMProviderGitHub,
		Host:           "github.com",
		Repo:           "aoagents/agent-orchestrator",
		Branch:         "feat/27",
		BaseBranch:     "main",
		CredentialHash: "cred",
		PRNumber:       28,
		PRURL:          "https://github.com/aoagents/agent-orchestrator/pull/28",
	}
}

func sampleSCMSnapshot(subject domain.SCMSubject, observedAt time.Time) domain.SCMSnapshot {
	return domain.SCMSnapshot{
		SessionID:  subject.SessionID,
		Subject:    subject,
		Freshness:  domain.SCMFreshnessFresh,
		ObservedAt: observedAt,
		PR: &domain.SCMPullRequest{
			ID:           subject.ChangeRequestID(),
			Number:       subject.PRNumber,
			URL:          subject.PRURL,
			Title:        "SCM",
			State:        domain.PROpen,
			SourceBranch: subject.Branch,
			TargetBranch: subject.BaseBranch,
			HeadSHA:      "abc123",
		},
		CI: domain.SCMCI{Summary: "passing", Checks: []domain.SCMCheck{{Name: "build", Status: "completed", Conclusion: "success"}}},
		Review: domain.SCMReview{
			Decision: "approved",
			UnresolvedThreads: []domain.SCMReviewThread{{
				ID:   "thread-1",
				Path: "main.go",
				Line: 10,
				URL:  "https://github.com/review/thread-1",
				Comments: []domain.SCMReviewComment{{
					ID: "c1", Author: "reviewer", Body: "nit", URL: "https://github.com/comment/c1", ThreadID: "thread-1",
				}},
			}},
		},
		Mergeability: domain.SCMMergeability{Mergeable: true, CIPassing: true, Approved: true, NoConflicts: true, RawState: "MERGEABLE"},
	}
}

func TestSCMStoreSaveSnapshotRevisionAndSemanticHash(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	rec := seedSCMSession(t, s)
	now := time.Now().UTC().Truncate(time.Second)
	subj := sampleSCMSubject(rec.ID)

	first, changed, err := s.SaveSnapshot(ctx, sampleSCMSnapshot(subj, now))
	if err != nil {
		t.Fatal(err)
	}
	if !changed || first.Revision != 1 || first.SemanticHash == "" {
		t.Fatalf("first save changed=%v rev=%d hash=%q", changed, first.Revision, first.SemanticHash)
	}
	secondSnap := sampleSCMSnapshot(subj, now.Add(time.Minute))
	second, changed, err := s.SaveSnapshot(ctx, secondSnap)
	if err != nil {
		t.Fatal(err)
	}
	if changed || second.Revision != 1 || second.SemanticHash != first.SemanticHash {
		t.Fatalf("observed-at only save should reuse rev/hash, changed=%v second=%+v first=%+v", changed, second, first)
	}
	modified := sampleSCMSnapshot(subj, now.Add(2*time.Minute))
	modified.CI.Summary = "failing"
	modified.CI.Checks[0].Conclusion = "failure"
	third, changed, err := s.SaveSnapshot(ctx, modified)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || third.Revision != 2 || third.SemanticHash == first.SemanticHash {
		t.Fatalf("semantic change should increment revision: changed=%v third=%+v firstHash=%s", changed, third, first.SemanticHash)
	}
	latest, ok, err := s.GetLatestSnapshot(ctx, rec.ID)
	if err != nil || !ok || latest.Revision != 2 || latest.CI.Summary != "failing" {
		t.Fatalf("latest = %+v ok=%v err=%v", latest, ok, err)
	}
}

func TestSCMStoreProviderCacheScopedAndPruned(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	scope := domain.SCMProviderCacheScope{Provider: domain.SCMProviderGitHub, Host: "github.com", Repo: "AOAgents/Agent-Orchestrator", CredentialHash: "a"}
	for i, key := range []string{"branch/a", "branch/b", "branch/c"} {
		entry := domain.SCMProviderCacheEntry{
			Key:        domain.SCMProviderCacheKey{SCMProviderCacheScope: scope, Namespace: "branch-map", Key: key},
			Value:      json.RawMessage(`{"ok":true}`),
			UpdatedAt:  time.Now().Add(time.Duration(i) * time.Second),
			MaxEntries: 2,
		}
		if err := s.PutProviderCache(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, err := s.GetProviderCache(ctx, domain.SCMProviderCacheKey{SCMProviderCacheScope: scope, Namespace: "branch-map", Key: "branch/a"}); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("oldest branch-map entry should be pruned")
	}
	if _, ok, _ := s.GetProviderCache(ctx, domain.SCMProviderCacheKey{SCMProviderCacheScope: scope, Namespace: "branch-map", Key: "branch/c"}); !ok {
		t.Fatal("newest branch-map entry should remain")
	}
	otherCred := scope
	otherCred.CredentialHash = "b"
	if err := s.PutProviderCache(ctx, domain.SCMProviderCacheEntry{
		Key:   domain.SCMProviderCacheKey{SCMProviderCacheScope: otherCred, Namespace: "branch-map", Key: "branch/c"},
		Value: json.RawMessage(`{"other":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProviderCache(ctx, domain.SCMProviderCachePrefix{SCMProviderCacheScope: scope, Namespace: "branch-map"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetProviderCache(ctx, domain.SCMProviderCacheKey{SCMProviderCacheScope: scope, Namespace: "branch-map", Key: "branch/c"}); ok {
		t.Fatal("delete prefix should remove only matching credential scope")
	}
	if _, ok, _ := s.GetProviderCache(ctx, domain.SCMProviderCacheKey{SCMProviderCacheScope: otherCred, Namespace: "branch-map", Key: "branch/c"}); !ok {
		t.Fatal("different credential scope should remain")
	}
}

func TestSCMStorePollStateAndSnapshotCDC(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	rec := seedSCMSession(t, s)
	key := domain.SCMPollStateKey{Provider: domain.SCMProviderGitHub, Host: "github.com", Repo: "aoagents/agent-orchestrator"}
	state := domain.SCMPollState{
		Key:             key,
		ConsecutiveFail: 2,
		BackoffUntil:    time.Now().UTC().Add(time.Minute).Truncate(time.Second),
		LastError:       &domain.SCMError{Kind: domain.SCMErrorRateLimited, Operation: "observe", Message: "slow down"},
	}
	if err := s.PutPollState(ctx, state); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetPollState(ctx, key)
	if err != nil || !ok || got.ConsecutiveFail != 2 || got.LastError == nil || got.LastError.Kind != domain.SCMErrorRateLimited {
		t.Fatalf("poll state = %+v ok=%v err=%v", got, ok, err)
	}
	if _, changed, err := s.SaveSnapshot(ctx, sampleSCMSnapshot(sampleSCMSubject(rec.ID), time.Now().UTC())); err != nil || !changed {
		t.Fatalf("save snapshot changed=%v err=%v", changed, err)
	}
	rows, err := s.ReadChangeLogAfter(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.EventType == "scm_snapshot_created" && row.SessionID == string(rec.ID) {
			found = true
			if !stringsContainsAll(row.Payload, `"revision":1`, `"semanticHash"`) {
				t.Fatalf("snapshot event payload missing fields: %s", row.Payload)
			}
		}
	}
	if !found {
		t.Fatalf("missing scm_snapshot_created event in %+v", rows)
	}
}

func TestPRCommentsPreserveSCMThreadMetadata(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	rec := seedSCMSession(t, s)
	now := time.Now().UTC().Truncate(time.Second)
	url := "https://github.com/o/r/pull/1"
	if err := s.WritePRObservation(ctx,
		PRRow{URL: url, SessionID: string(rec.ID), Number: 1, State: "open", UpdatedAt: now},
		nil,
		[]PRCommentRow{{
			PRURL: url, CommentID: "c1", Author: "renovate[bot]", File: "go.mod", Line: 7,
			Body: "pin this", ThreadID: "thread-1", URL: "https://github.com/comment/c1", IsBot: true, CreatedAt: now,
		}},
	); err != nil {
		t.Fatal(err)
	}
	comments, err := s.ListPRComments(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].ThreadID != "thread-1" || comments[0].URL == "" || !comments[0].IsBot {
		t.Fatalf("comment metadata not preserved: %+v", comments)
	}
}

func stringsContainsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
