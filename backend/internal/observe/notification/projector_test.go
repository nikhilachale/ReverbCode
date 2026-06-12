package notification

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type fakeStore struct {
	sessions      map[domain.SessionID]domain.SessionRecord
	prs           map[string]domain.PullRequest
	comments      map[string][]domain.PullRequestComment
	notifications []domain.NotificationRecord
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		sessions: map[domain.SessionID]domain.SessionRecord{},
		prs:      map[string]domain.PullRequest{},
		comments: map[string][]domain.PullRequestComment{},
	}
}

func (f *fakeStore) CreateNotification(_ context.Context, rec domain.NotificationRecord) (domain.NotificationRecord, bool, error) {
	for _, existing := range f.notifications {
		if existing.Status == domain.NotificationUnread && existing.SessionID == rec.SessionID && existing.Type == rec.Type && existing.PRURL == rec.PRURL {
			return domain.NotificationRecord{}, false, nil
		}
	}
	f.notifications = append(f.notifications, rec)
	return rec, true, nil
}

func (f *fakeStore) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	rec, ok := f.sessions[id]
	return rec, ok, nil
}

func (f *fakeStore) GetPR(_ context.Context, url string) (domain.PullRequest, bool, error) {
	pr, ok := f.prs[url]
	return pr, ok, nil
}

func (f *fakeStore) ListPRComments(_ context.Context, prURL string) ([]domain.PullRequestComment, error) {
	return f.comments[prURL], nil
}

func TestProjectorCreatesNeedsInputFromSessionCDC(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", DisplayName: "checkout-flow"}
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	p := New(Deps{Store: st, NewID: func() string { return "ntf_1" }})

	if err := p.Handle(context.Background(), event(t, 1, cdc.EventSessionUpdated, "mer", "mer-1", now, map[string]any{
		"id": "mer-1", "activity": string(domain.ActivityWaitingInput), "isTerminated": false,
	})); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(st.notifications) != 1 {
		t.Fatalf("notifications = %+v", st.notifications)
	}
	got := st.notifications[0]
	if got.Type != domain.NotificationNeedsInput || got.Title != "checkout-flow needs input" || got.CreatedAt != now {
		t.Fatalf("notification = %+v", got)
	}
}

func TestProjectorCreatesPRNotificationsFromPRCDC(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", DisplayName: "checkout-flow"}
	st.prs["pr1"] = domain.PullRequest{URL: "pr1", SessionID: "mer-1", Number: 42, Title: "checkout", CI: domain.CIPassing, Review: domain.ReviewApproved, Mergeability: domain.MergeMergeable}
	p := New(Deps{Store: st, NewID: func() string { return "ntf_1" }})

	if err := p.Handle(context.Background(), event(t, 1, cdc.EventPRUpdated, "mer", "mer-1", time.Now(), map[string]any{"url": "pr1"})); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(st.notifications) != 1 || st.notifications[0].Type != domain.NotificationReadyToMerge || st.notifications[0].Title != "PR #42 is ready to merge" {
		t.Fatalf("notifications = %+v", st.notifications)
	}
}

func TestProjectorDoesNotCreateReadyNotificationWhenCIIsPending(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer"}
	st.prs["pr1"] = domain.PullRequest{URL: "pr1", SessionID: "mer-1", Number: 42, CI: domain.CIPending, Review: domain.ReviewApproved, Mergeability: domain.MergeMergeable}
	p := New(Deps{Store: st})

	if err := p.Handle(context.Background(), event(t, 1, cdc.EventPRUpdated, "mer", "mer-1", time.Now(), map[string]any{"url": "pr1"})); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(st.notifications) != 0 {
		t.Fatalf("notifications = %+v", st.notifications)
	}
}

func TestProjectorCreatesMergedNotificationForTerminatedSession(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", IsTerminated: true}
	st.prs["pr1"] = domain.PullRequest{URL: "pr1", SessionID: "mer-1", Number: 42, Merged: true}
	p := New(Deps{Store: st, NewID: func() string { return "ntf_1" }})

	if err := p.Handle(context.Background(), event(t, 1, cdc.EventPRUpdated, "mer", "mer-1", time.Now(), map[string]any{"url": "pr1"})); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(st.notifications) != 1 || st.notifications[0].Type != domain.NotificationPRMerged {
		t.Fatalf("notifications = %+v", st.notifications)
	}
}

func event(t *testing.T, seq int64, typ cdc.EventType, projectID, sessionID string, at time.Time, payload map[string]any) cdc.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return cdc.Event{Seq: seq, ProjectID: projectID, SessionID: sessionID, Type: typ, Payload: raw, CreatedAt: at}
}
