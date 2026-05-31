package observer

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const DefaultSchedulerInterval = 30 * time.Second

type Scheduler struct {
	Observer ports.SCMObserver
	Store    ports.SCMStore
	Projects ProjectSource
	Sessions SessionSource
	PRs      PRBindingSource
	Interval time.Duration
	Clock    func() time.Time
	Logger   *slog.Logger
}

func (s *Scheduler) Name() string { return "scm" }

type ProjectSource interface {
	ListSCMProjects(ctx context.Context) ([]ProjectConfig, error)
}

type SessionSource interface {
	ListSCMSessions(ctx context.Context) ([]domain.SessionRecord, error)
}

type PRBindingSource interface {
	LatestPRBinding(ctx context.Context, sessionID domain.SessionID) (PRBinding, bool, error)
}

type ProjectConfig struct {
	ID            domain.ProjectID
	Path          string
	RepoOriginURL string
	DefaultBranch string
	SCMProvider   domain.SCMProvider
	SCMHost       string
	SCMRepo       string
}

type PRBinding struct {
	Number int
	URL    string
}

func (s *Scheduler) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Poll(ctx)
		interval := s.Interval
		if interval <= 0 {
			interval = DefaultSchedulerInterval
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.Poll(ctx); err != nil && s.Logger != nil {
					s.Logger.Error("scm scheduler: poll failed", "err", err)
				}
			}
		}
	}()
	return done
}

func (s *Scheduler) Poll(ctx context.Context) error {
	if s.Observer == nil || s.Store == nil || s.Projects == nil || s.Sessions == nil {
		return nil
	}
	now := time.Now()
	if s.Clock != nil {
		now = s.Clock()
	}
	subjects, err := s.DeriveSubjects(ctx)
	if err != nil {
		return err
	}
	due := make([]domain.SCMSubject, 0, len(subjects))
	for _, subj := range subjects {
		if err := s.Store.UpsertSubject(ctx, subj); err != nil {
			return err
		}
		if s.pollBlocked(ctx, subj, now) {
			continue
		}
		due = append(due, subj)
	}
	if len(due) == 0 {
		return nil
	}
	return s.Observer.Refresh(ctx, due)
}

func (s *Scheduler) DeriveSubjects(ctx context.Context) ([]domain.SCMSubject, error) {
	projects, err := s.Projects.ListSCMProjects(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[domain.ProjectID]ProjectConfig{}
	for _, p := range projects {
		if p.DefaultBranch == "" {
			p.DefaultBranch = "main"
		}
		if p.SCMProvider == "" || p.SCMHost == "" || p.SCMRepo == "" {
			resolved, ok := ResolveGitHubProject(p)
			if !ok {
				continue
			}
			p = resolved
		}
		byID[p.ID] = p
	}
	sessions, err := s.Sessions.ListSCMSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.SCMSubject, 0, len(sessions))
	for _, rec := range sessions {
		if isTerminalSession(rec.Lifecycle.Session.State) || strings.TrimSpace(rec.Metadata.Branch) == "" {
			continue
		}
		p, ok := byID[rec.ProjectID]
		if !ok {
			continue
		}
		subj := domain.SCMSubject{
			SessionID:  rec.ID,
			ProjectID:  rec.ProjectID,
			Provider:   p.SCMProvider,
			Host:       p.SCMHost,
			Repo:       p.SCMRepo,
			Branch:     rec.Metadata.Branch,
			BaseBranch: p.DefaultBranch,
		}
		if existing, ok, err := s.Store.GetSubject(ctx, rec.ID); err != nil {
			return nil, err
		} else if ok {
			subj.CredentialHash = existing.CredentialHash
			subj.PRNumber = existing.PRNumber
			subj.PRURL = existing.PRURL
			subj.CreatedAt = existing.CreatedAt
		}
		if subj.PRNumber == 0 && s.PRs != nil {
			if binding, ok, err := s.PRs.LatestPRBinding(ctx, rec.ID); err != nil {
				return nil, err
			} else if ok {
				subj.PRNumber = binding.Number
				subj.PRURL = binding.URL
			}
		}
		out = append(out, subj)
	}
	return out, nil
}

func (s *Scheduler) pollBlocked(ctx context.Context, subj domain.SCMSubject, now time.Time) bool {
	st, ok, err := s.Store.GetPollState(ctx, domain.SCMPollStateKey{Provider: subj.Provider, Host: subj.Host, Repo: subj.Repo})
	if err != nil || !ok {
		return false
	}
	return (!st.BackoffUntil.IsZero() && now.Before(st.BackoffUntil)) ||
		(!st.RateLimitUntil.IsZero() && now.Before(st.RateLimitUntil))
}

func ResolveGitHubProject(p ProjectConfig) (ProjectConfig, bool) {
	raw := strings.TrimSpace(p.RepoOriginURL)
	if raw == "" && p.Path != "" {
		raw = gitRemoteOrigin(p.Path)
	}
	host, repo, ok := parseGitHubRemote(raw)
	if !ok {
		return ProjectConfig{}, false
	}
	p.SCMProvider = domain.SCMProviderGitHub
	p.SCMHost = host
	p.SCMRepo = repo
	if p.DefaultBranch == "" {
		p.DefaultBranch = "main"
	}
	return p, true
}

func gitRemoteOrigin(repoPath string) string {
	out, err := exec.Command("git", "-C", repoPath, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var scpLikeRemote = regexp.MustCompile(`^(?:[^@]+@)?([^:]+):(.+)$`)

func parseGitHubRemote(raw string) (host, repo string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	if !strings.Contains(raw, "://") {
		if m := scpLikeRemote.FindStringSubmatch(raw); len(m) == 3 && !strings.Contains(m[1], "/") {
			host, repo = m[1], strings.TrimSuffix(m[2], ".git")
			return normalizeSchedulerHost(host), normalizeSchedulerRepo(repo), strings.EqualFold(normalizeSchedulerHost(host), "github.com") && repo != ""
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	host = u.Host
	repo = strings.TrimPrefix(path.Clean(u.Path), "/")
	repo = strings.TrimSuffix(repo, ".git")
	if strings.Count(repo, "/") < 1 {
		return "", "", false
	}
	host = normalizeSchedulerHost(host)
	return host, normalizeSchedulerRepo(repo), strings.EqualFold(host, "github.com")
}

func normalizeSchedulerHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	return strings.TrimSuffix(host, "/")
}

func normalizeSchedulerRepo(repo string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(repo), "/"))
}

func (s *Scheduler) String() string {
	return fmt.Sprintf("scm scheduler interval=%s", s.Interval)
}
