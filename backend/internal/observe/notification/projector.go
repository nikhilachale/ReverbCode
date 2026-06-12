// Package notification projects durable CDC facts into unread notification rows.
package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const defaultQueueSize = 1024

// Store is the notification projector's storage boundary.
type Store interface {
	CreateNotification(ctx context.Context, rec domain.NotificationRecord) (domain.NotificationRecord, bool, error)
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
	GetPR(ctx context.Context, url string) (domain.PullRequest, bool, error)
	ListPRComments(ctx context.Context, prURL string) ([]domain.PullRequestComment, error)
}

// Subscriber is the live CDC stream consumed by the projector.
type Subscriber interface {
	Subscribe(func(cdc.Event)) (unsubscribe func())
}

// Projector consumes session/PR CDC facts and writes durable unread notifications.
type Projector struct {
	store  Store
	live   Subscriber
	logger *slog.Logger
	clock  func() time.Time
	newID  func() string
	queue  int
}

// Deps configures a Projector.
type Deps struct {
	Store  Store
	Live   Subscriber
	Logger *slog.Logger
	Clock  func() time.Time
	NewID  func() string
	Queue  int
}

// New constructs a notification CDC projector.
func New(d Deps) *Projector {
	p := &Projector{store: d.Store, live: d.Live, logger: d.Logger, clock: d.Clock, newID: d.NewID, queue: d.Queue}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	if p.clock == nil {
		p.clock = time.Now
	}
	if p.newID == nil {
		p.newID = func() string { return "ntf_" + uuid.NewString() }
	}
	if p.queue <= 0 {
		p.queue = defaultQueueSize
	}
	return p
}

// Start subscribes to live CDC events and processes them until ctx is cancelled.
func (p *Projector) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	if p == nil || p.live == nil || p.store == nil {
		close(done)
		return done
	}
	events := make(chan cdc.Event, p.queue)
	unsubscribe := p.live.Subscribe(func(e cdc.Event) {
		select {
		case events <- e:
		default:
			p.logger.Warn("notification projector: queue full; dropping CDC event", "seq", e.Seq, "type", e.Type)
		}
	})
	go func() {
		defer close(done)
		defer unsubscribe()
		for {
			select {
			case <-ctx.Done():
				return
			case e := <-events:
				if err := p.Handle(ctx, e); err != nil && !errors.Is(err, context.Canceled) {
					p.logger.Warn("notification projector: event failed", "seq", e.Seq, "type", e.Type, "err", err)
				}
			}
		}
	}()
	return done
}

// Handle projects a single CDC event. It is exported for deterministic tests.
func (p *Projector) Handle(ctx context.Context, e cdc.Event) error {
	if p == nil || p.store == nil {
		return errors.New("notification projector: store is required")
	}
	switch e.Type {
	case cdc.EventSessionUpdated:
		return p.handleSessionUpdated(ctx, e)
	case cdc.EventPRCreated, cdc.EventPRUpdated, cdc.EventPRSessionChanged, cdc.EventPRReviewThreadResolved:
		return p.handlePRChanged(ctx, e)
	default:
		return nil
	}
}

func (p *Projector) handleSessionUpdated(ctx context.Context, e cdc.Event) error {
	var payload struct {
		Activity     string `json:"activity"`
		IsTerminated bool   `json:"isTerminated"`
	}
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		return fmt.Errorf("decode session event payload: %w", err)
	}
	if domain.ActivityState(payload.Activity) != domain.ActivityWaitingInput || payload.IsTerminated || e.SessionID == "" {
		return nil
	}
	rec, ok, err := p.store.GetSession(ctx, domain.SessionID(e.SessionID))
	if err != nil || !ok {
		return err
	}
	return p.create(ctx, intent{
		Type:               domain.NotificationNeedsInput,
		SessionID:          rec.ID,
		ProjectID:          rec.ProjectID,
		CreatedAt:          timeOr(e.CreatedAt, p.clock()),
		SessionDisplayName: rec.DisplayName,
	})
}

func (p *Projector) handlePRChanged(ctx context.Context, e cdc.Event) error {
	prURL, err := prURLFromEvent(e)
	if err != nil || prURL == "" {
		return err
	}
	pr, ok, err := p.store.GetPR(ctx, prURL)
	if err != nil || !ok {
		return err
	}
	rec, ok, err := p.store.GetSession(ctx, pr.SessionID)
	if err != nil || !ok {
		return err
	}
	comments, err := p.store.ListPRComments(ctx, pr.URL)
	if err != nil {
		return err
	}
	in := intentForPR(rec, pr, comments, timeOr(e.CreatedAt, p.clock()))
	if in == nil {
		return nil
	}
	return p.create(ctx, *in)
}

func (p *Projector) create(ctx context.Context, in intent) error {
	rec, err := enrich(in)
	if err != nil {
		return err
	}
	rec.ID = p.newID()
	_, _, err = p.store.CreateNotification(ctx, rec)
	return err
}

func prURLFromEvent(e cdc.Event) (string, error) {
	var payload struct {
		URL string `json:"url"`
		PR  string `json:"pr"`
	}
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		return "", fmt.Errorf("decode pr event payload: %w", err)
	}
	return firstNonEmpty(payload.URL, payload.PR), nil
}

func intentForPR(rec domain.SessionRecord, pr domain.PullRequest, comments []domain.PullRequestComment, createdAt time.Time) *intent {
	base := intent{
		SessionID:          rec.ID,
		ProjectID:          rec.ProjectID,
		PRURL:              firstNonEmpty(pr.URL, pr.HTMLURL),
		CreatedAt:          createdAt,
		SessionDisplayName: rec.DisplayName,
		PRNumber:           pr.Number,
		PRTitle:            pr.Title,
		PRSourceBranch:     pr.SourceBranch,
		PRTargetBranch:     pr.TargetBranch,
		Provider:           pr.Provider,
		Repo:               pr.Repo,
	}
	if pr.Merged {
		base.Type = domain.NotificationPRMerged
		return &base
	}
	if pr.Closed {
		base.Type = domain.NotificationPRClosedUnmerged
		return &base
	}
	if rec.IsTerminated || rec.Activity.State == domain.ActivityWaitingInput || !prIsReadyToMerge(pr, comments) {
		return nil
	}
	base.Type = domain.NotificationReadyToMerge
	return &base
}

func prIsReadyToMerge(pr domain.PullRequest, comments []domain.PullRequestComment) bool {
	if pr.Merged || pr.Closed || pr.Draft {
		return false
	}
	if pr.CI != domain.CIPassing {
		return false
	}
	if pr.Review == domain.ReviewChangesRequest || hasUnresolvedReviewComments(comments) {
		return false
	}
	return pr.Mergeability == domain.MergeMergeable
}

func hasUnresolvedReviewComments(comments []domain.PullRequestComment) bool {
	for _, c := range comments {
		if !c.Resolved && !c.IsBot {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func timeOr(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t
}
