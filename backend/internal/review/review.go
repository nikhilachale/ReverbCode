// Package review holds the core code-review logic: triggering a reviewer over a
// worker's worktree, recording review runs, and accepting submitted results.
//
// It is independent of any transport. The daemon's HTTP service
// (internal/service/review) is a thin boundary over this engine today, and the
// same engine can back an in-process CLI trigger later without going through the
// API. Transport-specific concerns (DTOs, error→status mapping) stay in the
// service/controller layers; the orchestration and run-id generation live here.
package review

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrInvalid and ErrNotFound let the transport layer map failures to 422/404.
var (
	ErrInvalid  = errors.New("review: invalid input")
	ErrNotFound = errors.New("review: not found")
)

// Store is the persistence surface the engine needs. *sqlite.Store satisfies it
// in production; tests use a fake.
type Store interface {
	UpsertReview(ctx context.Context, r domain.Review) error
	GetReviewBySession(ctx context.Context, id domain.SessionID) (domain.Review, bool, error)
	InsertReviewRun(ctx context.Context, r domain.ReviewRun) error
	UpdateReviewRunResult(ctx context.Context, id string, status domain.ReviewRunStatus, verdict domain.ReviewVerdict, body string) (bool, error)
	GetReviewRun(ctx context.Context, id string) (domain.ReviewRun, bool, error)
	GetLatestReviewRunBySession(ctx context.Context, id domain.SessionID) (domain.ReviewRun, bool, error)
	ListReviewRunsBySession(ctx context.Context, id domain.SessionID) ([]domain.ReviewRun, error)
}

// Sessions resolves the worker session under review.
type Sessions interface {
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
}

// PRs resolves the PR a worker owns.
type PRs interface {
	ListPRsBySession(ctx context.Context, id domain.SessionID) ([]domain.PullRequest, error)
}

// Projects resolves the per-project reviewer config.
type Projects interface {
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
}

// Runner launches the reviewer one-shot over the worker's worktree.
type Runner interface {
	Run(ctx context.Context, spec RunSpec) error
}

// RunSpec describes one reviewer launch.
type RunSpec struct {
	RunID         string
	WorkerID      domain.SessionID
	Harness       domain.ReviewerHarness
	WorkspacePath string
	PRURL         string
}

// Deps wires the engine.
type Deps struct {
	Store    Store
	Sessions Sessions
	PRs      PRs
	Projects Projects
	Runner   Runner

	// Clock and NewID are injectable for deterministic tests.
	Clock func() time.Time
	NewID func() string
}

// Engine is the core code-review engine.
type Engine struct {
	store    Store
	sessions Sessions
	prs      PRs
	projects Projects
	runner   Runner
	clock    func() time.Time
	newID    func() string
}

// New wires an Engine from its dependencies, defaulting the clock and id source.
func New(d Deps) *Engine {
	clock := d.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	newID := d.NewID
	if newID == nil {
		newID = uuid.NewString
	}
	return &Engine{
		store:    d.Store,
		sessions: d.Sessions,
		prs:      d.PRs,
		projects: d.Projects,
		runner:   d.Runner,
		clock:    clock,
		newID:    newID,
	}
}

// Trigger starts a review pass for a worker's PR: it reuses (or creates) the
// worker's review row, records a running review_run (whose id is the run id),
// and launches the configured reviewer over the worker's worktree.
func (e *Engine) Trigger(ctx context.Context, workerID domain.SessionID) (domain.ReviewRun, error) {
	if workerID == "" {
		return domain.ReviewRun{}, fmt.Errorf("%w: worker session id is required", ErrInvalid)
	}
	worker, ok, err := e.sessions.GetSession(ctx, workerID)
	if err != nil {
		return domain.ReviewRun{}, err
	}
	if !ok {
		return domain.ReviewRun{}, fmt.Errorf("%w: worker session %q", ErrNotFound, workerID)
	}
	if worker.IsTerminated {
		return domain.ReviewRun{}, fmt.Errorf("%w: worker session %q is terminated", ErrInvalid, workerID)
	}
	if worker.Metadata.WorkspacePath == "" {
		return domain.ReviewRun{}, fmt.Errorf("%w: worker session %q has no workspace to review", ErrInvalid, workerID)
	}

	prURL, err := e.workerPRURL(ctx, workerID)
	if err != nil {
		return domain.ReviewRun{}, err
	}

	harness, err := e.reviewerHarness(ctx, worker)
	if err != nil {
		return domain.ReviewRun{}, err
	}

	now := e.clock()
	iteration := e.nextIteration(ctx, workerID)

	review, err := e.upsertReview(ctx, worker, harness, prURL, now)
	if err != nil {
		return domain.ReviewRun{}, err
	}
	run := domain.ReviewRun{
		ID:        e.newID(),
		ReviewID:  review.ID,
		SessionID: workerID,
		Harness:   harness,
		PRURL:     prURL,
		Status:    domain.ReviewRunRunning,
		Verdict:   domain.VerdictNone,
		Iteration: iteration,
		CreatedAt: now,
	}
	if err := e.store.InsertReviewRun(ctx, run); err != nil {
		return domain.ReviewRun{}, err
	}
	runErr := e.runner.Run(ctx, RunSpec{
		RunID:         run.ID,
		WorkerID:      workerID,
		Harness:       harness,
		WorkspacePath: worker.Metadata.WorkspacePath,
		PRURL:         prURL,
	})
	if runErr != nil {
		if _, err := e.store.UpdateReviewRunResult(ctx, run.ID, domain.ReviewRunFailed, domain.VerdictNone, ""); err != nil {
			return domain.ReviewRun{}, err
		}
		run.Status = domain.ReviewRunFailed
		return run, fmt.Errorf("launch reviewer: %w", runErr)
	}
	return run, nil
}

// Submit records the reviewer's result for a specific worker review pass: it
// marks the run complete and stores the verdict and body. AO does not post the
// review — the reviewer agent posts it to the PR itself.
func (e *Engine) Submit(ctx context.Context, workerID domain.SessionID, runID string, verdict domain.ReviewVerdict, body string) (domain.ReviewRun, error) {
	if workerID == "" {
		return domain.ReviewRun{}, fmt.Errorf("%w: worker session id is required", ErrInvalid)
	}
	if runID == "" {
		return domain.ReviewRun{}, fmt.Errorf("%w: review run id is required", ErrInvalid)
	}
	if !verdict.Valid() {
		return domain.ReviewRun{}, fmt.Errorf("%w: verdict must be %q or %q", ErrInvalid, domain.VerdictApproved, domain.VerdictChangesRequested)
	}
	if verdict == domain.VerdictChangesRequested && body == "" {
		return domain.ReviewRun{}, fmt.Errorf("%w: a changes_requested review requires a body", ErrInvalid)
	}

	run, ok, err := e.store.GetReviewRun(ctx, runID)
	if err != nil {
		return domain.ReviewRun{}, err
	}
	if !ok {
		return domain.ReviewRun{}, fmt.Errorf("%w: review run %q", ErrNotFound, runID)
	}
	if run.SessionID != workerID {
		return domain.ReviewRun{}, fmt.Errorf("%w: review run %q does not belong to worker %q", ErrInvalid, runID, workerID)
	}
	if run.Status != domain.ReviewRunRunning {
		return domain.ReviewRun{}, fmt.Errorf("%w: review run %q is not running", ErrInvalid, runID)
	}

	updated, err := e.store.UpdateReviewRunResult(ctx, run.ID, domain.ReviewRunComplete, verdict, body)
	if err != nil {
		return domain.ReviewRun{}, err
	}
	if !updated {
		return domain.ReviewRun{}, fmt.Errorf("%w: review run %q is not running", ErrInvalid, runID)
	}
	run.Status = domain.ReviewRunComplete
	run.Verdict = verdict
	run.Body = body
	return run, nil
}

// List returns the review passes recorded for a worker, newest first.
func (e *Engine) List(ctx context.Context, workerID domain.SessionID) ([]domain.ReviewRun, error) {
	if workerID == "" {
		return nil, fmt.Errorf("%w: worker session id is required", ErrInvalid)
	}
	return e.store.ListReviewRunsBySession(ctx, workerID)
}

func (e *Engine) workerPRURL(ctx context.Context, workerID domain.SessionID) (string, error) {
	prs, err := e.prs.ListPRsBySession(ctx, workerID)
	if err != nil {
		return "", err
	}
	if len(prs) == 0 {
		return "", fmt.Errorf("%w: worker %q has no PR to review", ErrInvalid, workerID)
	}
	return prs[0].URL, nil
}

// reviewerHarness resolves which harness reviews the worker's PR: a configured
// reviewer wins, otherwise the worker's own harness is reused (falling back to
// claude-code), per domain.ResolveReviewerHarness.
func (e *Engine) reviewerHarness(ctx context.Context, worker domain.SessionRecord) (domain.ReviewerHarness, error) {
	var cfg domain.ProjectConfig
	if e.projects != nil {
		if proj, ok, err := e.projects.GetProject(ctx, string(worker.ProjectID)); err != nil {
			return "", err
		} else if ok {
			cfg = proj.Config
		}
	}
	return cfg.ResolveReviewerHarness(worker.Harness), nil
}

func (e *Engine) upsertReview(ctx context.Context, worker domain.SessionRecord, harness domain.ReviewerHarness, prURL string, now time.Time) (domain.Review, error) {
	existing, ok, err := e.store.GetReviewBySession(ctx, worker.ID)
	if err != nil {
		return domain.Review{}, err
	}
	review := domain.Review{
		ID:        e.newID(),
		SessionID: worker.ID,
		ProjectID: worker.ProjectID,
		Harness:   harness,
		PRURL:     prURL,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if ok {
		// Reuse the existing row's identity and creation time; UpsertReview
		// refreshes harness/pr_url/updated_at.
		review.ID = existing.ID
		review.CreatedAt = existing.CreatedAt
	}
	if err := e.store.UpsertReview(ctx, review); err != nil {
		return domain.Review{}, err
	}
	return review, nil
}

func (e *Engine) nextIteration(ctx context.Context, workerID domain.SessionID) int {
	if latest, ok, err := e.store.GetLatestReviewRunBySession(ctx, workerID); err == nil && ok {
		return latest.Iteration + 1
	}
	return 1
}
