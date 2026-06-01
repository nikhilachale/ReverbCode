package lifecycle

// reactions.go is the ACT layer: after a persisted transition the engine maps
// the session's (state, PR facts) to at most one agent nudge. Two reactions
// inject live content (CI logs, review comments) and re-fire when that content
// changes; the rest fire once on entry.
//
// Budgets are in-memory: a restart re-arms them, which costs a few extra nudges,
// never a missed page.

import (
	"context"
	"strings"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type reactionKey string

const (
	rxCIFailed       reactionKey = "ci-failed"
	rxReviewComments reactionKey = "review-comments"
	rxMergeConflicts reactionKey = "merge-conflicts"
	rxIdle           reactionKey = "agent-idle"
)

// Brakes: stop auto-handling after this many failed attempts.
const (
	ciBrakeRuns    = 3 // last N runs of a failing check all failed
	reviewMaxNudge = 3 // re-nudged the agent N times over new review feedback
)

// reactionConfig is one row of the reaction table.
type reactionConfig struct {
	message string
}

var reactions = map[reactionKey]reactionConfig{
	rxCIFailed:       {message: "CI is failing on your PR. Review the output below and push a fix."},
	rxReviewComments: {message: "A reviewer left feedback on your PR. Address it and push."},
	rxMergeConflicts: {message: "Your PR has merge conflicts. Rebase onto the base branch and resolve them."},
	rxIdle:           {message: "You appear idle. Continue the task or say what is blocking you."},
}

// reactionContent carries the live material the feedback reactions inject. Empty
// for runtime/activity transitions; populated from a PR observation.
type reactionContent struct {
	ciCheck   string
	ciCommit  string
	ciURL     string
	ciLogTail string
	comments  []string
	reviewSig string
}

// prContent extracts the CI failure + review feedback from a PR observation.
func prContent(o ports.PRObservation) reactionContent {
	c := reactionContent{}
	for _, ch := range o.Checks {
		if ch.Status == domain.PRCheckFailed {
			c.ciCheck, c.ciCommit, c.ciLogTail, c.ciURL = ch.Name, ch.CommitHash, ch.LogTail, o.URL
			break
		}
	}
	var ids []string
	for _, cm := range o.Comments {
		if cm.Resolved {
			continue
		}
		c.comments = append(c.comments, cm.Body)
		ids = append(ids, cm.ID)
	}
	c.reviewSig = strings.Join(ids, ",")
	return c
}

// ---- in-memory escalation state ----

type trackerKey struct {
	id  domain.SessionID
	key reactionKey
}

type tracker struct {
	attempts  int
	exhausted bool
	seenSig   bool
	lastSig   string
}

type reactionState struct {
	mu       sync.Mutex
	trackers map[trackerKey]*tracker
	lastKey  map[domain.SessionID]reactionKey
}

func newReactionState() reactionState {
	return reactionState{trackers: map[trackerKey]*tracker{}, lastKey: map[domain.SessionID]reactionKey{}}
}

// trackerFor returns the (id,key) tracker, creating it on first use. Caller holds mu.
func (rs *reactionState) trackerFor(id domain.SessionID, key reactionKey) *tracker {
	k := trackerKey{id, key}
	t := rs.trackers[k]
	if t == nil {
		t = &tracker{}
		rs.trackers[k] = t
	}
	return t
}

func (m *Manager) clearReactions(id domain.SessionID) {
	m.react.mu.Lock()
	defer m.react.mu.Unlock()
	for k := range m.react.trackers {
		if k.id == id {
			delete(m.react.trackers, k)
		}
	}
	delete(m.react.lastKey, id)
}

// ---- dispatch ----

// runReactions is the chokepoint called after every persisted transition. It
// runs unlocked (the write lock is already released) so a busy agent send never
// blocks the write path.
func (m *Manager) runReactions(ctx context.Context, id domain.SessionID, content reactionContent) error {
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil || !ok {
		return err
	}
	if rec.IsTerminated {
		m.clearReactions(id)
		return nil
	}

	pr, err := m.store.PRFactsForSession(ctx, id)
	if err != nil {
		return err
	}

	// Feedback reactions inject live content and re-fire as it changes — only
	// while the agent can actually act on it.
	if pr.Exists && !pr.Closed && !needsHuman(rec.Activity.State) {
		if pr.CI == domain.CIFailing && content.ciCheck != "" {
			if err := m.handleCIFailure(ctx, id, content); err != nil {
				return err
			}
		}
		if hasReviewFeedback(pr) {
			if err := m.handleReviewFeedback(ctx, id, content); err != nil {
				return err
			}
		}
	}

	return m.dispatch(ctx, id, reactionFor(rec, pr))
}

// dispatch fires the entry reaction for key, deduped so a steady state does not
// re-fire. Leaving a reaction drops its budget.
func (m *Manager) dispatch(ctx context.Context, id domain.SessionID, key reactionKey) error {
	m.react.mu.Lock()
	if m.react.lastKey[id] == key {
		m.react.mu.Unlock()
		return nil
	}
	if prev := m.react.lastKey[id]; prev != "" {
		delete(m.react.trackers, trackerKey{id, prev})
	}
	m.react.lastKey[id] = key
	m.react.mu.Unlock()

	if key == "" {
		return nil
	}
	return m.fireAgentEntry(ctx, id, key, reactions[key])
}

// reactionFor maps (session state, PR facts) to the reaction to enter. CI failure
// and review feedback return "" here — they are handled by the feedback path.
func reactionFor(rec domain.SessionRecord, pr domain.PRFacts) reactionKey {
	switch rec.Activity.State {
	case domain.ActivityBlocked, domain.ActivityWaitingInput:
		return ""
	}
	if pr.Exists {
		if pr.Closed {
			return ""
		}
		switch {
		case pr.CI == domain.CIFailing, hasReviewFeedback(pr):
			return "" // feedback path
		case pr.Mergeability == domain.MergeConflicting:
			return rxMergeConflicts
		}
	}
	if rec.Activity.State == domain.ActivityIdle || rec.Activity.State == domain.ActivityReady || rec.Activity.State == "" {
		return rxIdle
	}
	return ""
}

func hasReviewFeedback(pr domain.PRFacts) bool {
	return pr.Review == domain.ReviewChangesRequest || pr.ReviewComments
}

func needsHuman(s domain.ActivityState) bool {
	return s == domain.ActivityBlocked || s == domain.ActivityWaitingInput
}

// ---- feedback reactions (content-driven re-fire + brake) ----

func (m *Manager) handleCIFailure(ctx context.Context, id domain.SessionID, c reactionContent) error {
	msg := reactions[rxCIFailed].message + "\n\nFailing output:\n" + c.ciLogTail
	return m.fireFeedback(ctx, id, rxCIFailed, c.ciCommit, msg, func(int) (bool, error) {
		st, err := m.pr.RecentCheckStatuses(ctx, c.ciURL, c.ciCheck, ciBrakeRuns)
		if err != nil {
			return false, err
		}
		return allFailed(st, ciBrakeRuns), nil
	})
}

func (m *Manager) handleReviewFeedback(ctx context.Context, id domain.SessionID, c reactionContent) error {
	msg := reactions[rxReviewComments].message
	if len(c.comments) > 0 {
		msg += "\n\n" + strings.Join(c.comments, "\n\n")
	}
	return m.fireFeedback(ctx, id, rxReviewComments, c.reviewSig, msg, func(attempts int) (bool, error) {
		return attempts > reviewMaxNudge, nil
	})
}

// fireFeedback nudges the agent with fresh content, deduped by signature so the
// same content is not re-sent each poll. braked decides whether to stop
// retrying (CI: history; review: attempt count).
func (m *Manager) fireFeedback(ctx context.Context, id domain.SessionID, key reactionKey, sig, message string, braked func(attempts int) (bool, error)) error {
	m.react.mu.Lock()
	t := m.react.trackerFor(id, key)
	if t.exhausted || (t.seenSig && t.lastSig == sig) {
		m.react.mu.Unlock()
		return nil
	}
	t.seenSig, t.lastSig = true, sig
	t.attempts++
	attempts := t.attempts
	m.react.lastKey[id] = key // feedback owns the slot so a later dispatch("") clears it
	m.react.mu.Unlock()

	brake, err := braked(attempts)
	if err != nil {
		return err
	}
	if brake {
		m.react.mu.Lock()
		t.exhausted = true
		m.react.mu.Unlock()
		return nil
	}
	return m.messenger.Send(ctx, id, message)
}

// ---- entry reactions ----

// fireAgentEntry nudges the agent once on entry into a static reaction
// (idle/merge-conflicts).
func (m *Manager) fireAgentEntry(ctx context.Context, id domain.SessionID, key reactionKey, cfg reactionConfig) error {
	m.react.mu.Lock()
	t := m.react.trackerFor(id, key)
	if t.exhausted {
		m.react.mu.Unlock()
		return nil
	}
	t.attempts++
	m.react.mu.Unlock()
	return m.messenger.Send(ctx, id, cfg.message)
}

func allFailed(statuses []domain.PRCheckStatus, n int) bool {
	if len(statuses) < n {
		return false
	}
	for i := 0; i < n; i++ {
		if statuses[i] != domain.PRCheckFailed {
			return false
		}
	}
	return true
}
