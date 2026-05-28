package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/scm/store"
)

func TestRESTETag200And304(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("auth header %q", got)
		}
		w.Header().Set("ETag", `"v1"`)
		if calls.Add(1) == 2 {
			if got := r.Header.Get("If-None-Match"); got != `"v1"` {
				t.Fatalf("If-None-Match %q", got)
			}
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()
	c := NewClient(ClientOptions{RESTBase: ts.URL, GraphQLURL: ts.URL + "/graphql", Token: StaticTokenSource("token")})
	ctx := context.Background()
	resp, err := c.DoREST(ctx, http.MethodGet, "/x", nil, nil, "", "test")
	if err != nil || resp.NotModified || resp.ETag != `"v1"` {
		t.Fatalf("first resp=%+v err=%v", resp, err)
	}
	resp, err = c.DoREST(ctx, http.MethodGet, "/x", nil, nil, resp.ETag, "test")
	if err != nil || !resp.NotModified {
		t.Fatalf("second resp=%+v err=%v", resp, err)
	}
}

func TestBranchDiscoveryCachesPositiveMapping(t *testing.T) {
	var pullListCalls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
			pullListCalls.Add(1)
			w.Header().Set("ETag", `"pulls"`)
			_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 3, "html_url": "https://github.com/o/r/pull/3", "head": map[string]any{"ref": "feat/27"}, "base": map[string]any{"ref": "main"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			writeGraphQLPR(t, w, 3, "feat/27", "SUCCESS", "APPROVED", nil)
		case strings.Contains(r.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"check_runs":[]}`))
		case strings.Contains(r.URL.Path, "/comments"):
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ts.Close()
	st := store.NewMemoryStore()
	p := NewProvider(ProviderOptions{RESTBase: ts.URL, GraphQLURL: ts.URL + "/graphql", Token: StaticTokenSource("token")})
	subj := domain.SCMSubject{SessionID: "s1", ProjectID: "p1", Provider: domain.SCMProviderGitHub, Host: "github.com", Repo: "o/r", Branch: "feat/27"}
	res, err := p.ObserveSessions(context.Background(), observeReq(subj), st)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Subjects) != 1 || res.Subjects[0].PRNumber != 3 {
		t.Fatalf("discovered subjects=%+v", res.Subjects)
	}
	res, err = p.ObserveSessions(context.Background(), observeReq(subj), st)
	if err != nil {
		t.Fatal(err)
	}
	if pullListCalls.Load() != 1 {
		t.Fatalf("positive branch mapping did not avoid rediscovery; calls=%d", pullListCalls.Load())
	}
}

func TestGraphQLBatchNormalizationReviewAndMergeability(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			threads := []map[string]any{
				{"id": "human-thread", "isResolved": false, "path": "main.go", "line": 12, "comments": map[string]any{"nodes": []map[string]any{{"id": "c1", "body": "please change", "url": "https://example/comment", "author": map[string]any{"__typename": "User", "login": "alice"}}}}},
				{"id": "bot-thread", "isResolved": false, "path": "lint.go", "line": 1, "comments": map[string]any{"nodes": []map[string]any{{"id": "c2", "body": "lint", "url": "https://example/bot", "author": map[string]any{"__typename": "Bot", "login": "review-bot"}}}}},
			}
			writeGraphQLPR(t, w, 5, "feat/27", "SUCCESS", "APPROVED", threads)
		case strings.Contains(r.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"check_runs":[{"name":"test","status":"completed","conclusion":"success","html_url":"https://checks"}]}`))
		case strings.Contains(r.URL.Path, "/comments"):
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ts.Close()
	p := NewProvider(ProviderOptions{RESTBase: ts.URL, GraphQLURL: ts.URL + "/graphql", Token: StaticTokenSource("token")})
	subj := domain.SCMSubject{SessionID: "s1", ProjectID: "p1", Provider: domain.SCMProviderGitHub, Host: "github.com", Repo: "o/r", Branch: "feat/27", PRNumber: 5}
	res, err := p.ObserveSessions(context.Background(), observeReq(subj), store.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snapshots) != 1 {
		t.Fatalf("snapshots=%d", len(res.Snapshots))
	}
	s := res.Snapshots[0]
	if s.PR == nil || s.PR.State != domain.PROpen || s.CI.Summary != "passing" {
		t.Fatalf("bad pr/ci snapshot=%+v", s)
	}
	if len(s.Review.HumanComments) != 1 || len(s.Review.BotComments) != 1 {
		t.Fatalf("review split human=%d bot=%d", len(s.Review.HumanComments), len(s.Review.BotComments))
	}
	if !s.Mergeability.Mergeable {
		t.Fatalf("expected mergeable: %+v", s.Mergeability)
	}
}

func observeReq(subj domain.SCMSubject) ports.SCMObserveRequest {
	return ports.SCMObserveRequest{Subjects: []domain.SCMSubject{subj}, Now: time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)}
}

func writeGraphQLPR(t *testing.T, w http.ResponseWriter, number int, branch, ciState, reviewDecision string, threads []map[string]any) {
	t.Helper()
	if threads == nil {
		threads = []map[string]any{}
	}
	alias := "pr" + strconv.Itoa(number)
	resp := map[string]any{"data": map[string]any{"repository": map[string]any{alias: map[string]any{
		"number": number, "title": "PR", "url": "https://github.com/o/r/pull/" + strconv.Itoa(number), "state": "OPEN", "isDraft": false, "merged": false, "closed": false,
		"headRefName": branch, "baseRefName": "main", "headRefOid": "sha", "additions": 1, "deletions": 2, "mergeable": "MERGEABLE", "reviewDecision": reviewDecision, "mergeStateStatus": "CLEAN",
		"commits":       map[string]any{"nodes": []map[string]any{{"commit": map[string]any{"statusCheckRollup": map[string]any{"state": ciState, "contexts": map[string]any{"nodes": []map[string]any{{"__typename": "CheckRun", "name": "test", "status": "COMPLETED", "conclusion": "SUCCESS", "detailsUrl": "https://checks"}}}}}}}},
		"reviewThreads": map[string]any{"nodes": threads},
	}}, "rateLimit": map[string]any{"limit": 5000, "remaining": 4999, "resetAt": "2026-05-28T13:00:00Z"}}}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		t.Fatal(err)
	}
}
