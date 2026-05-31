package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/scm/observer"
)

// Consumer is the production seam between durable SCM snapshots and the LCM.
// The observer writes scm_snapshots; SQLite emits change_log rows; this consumer
// loads the latest snapshot and applies normalized facts. Provider adapters and
// the common observer do not call lifecycle directly in production.
type Consumer struct {
	Store  ports.SCMStore
	LCM    ports.LifecycleManager
	Logger *slog.Logger

	mu      sync.Mutex
	applied map[domain.SessionID]int64
}

func NewConsumer(store ports.SCMStore, lcm ports.LifecycleManager, logger *slog.Logger) *Consumer {
	return &Consumer{Store: store, LCM: lcm, Logger: logger, applied: map[domain.SessionID]int64{}}
}

func (c *Consumer) Handle(ctx context.Context, event cdc.Event) error {
	if event.Type != cdc.EventSCMSnapshotCreated {
		return nil
	}
	if c.Store == nil || c.LCM == nil {
		return nil
	}
	var payload struct {
		SessionID string `json:"sessionId"`
		Revision  int64  `json:"revision"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return fmt.Errorf("decode scm snapshot event %d: %w", event.Seq, err)
	}
	sessionID := domain.SessionID(payload.SessionID)
	if sessionID == "" {
		sessionID = domain.SessionID(event.SessionID)
	}
	if sessionID == "" {
		return fmt.Errorf("scm snapshot event %d missing session id", event.Seq)
	}
	snap, ok, err := c.Store.GetLatestSnapshot(ctx, sessionID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("scm snapshot event %d points at missing latest snapshot for %s", event.Seq, sessionID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.applied == nil {
		c.applied = map[domain.SessionID]int64{}
	}
	if snap.Revision > 0 && c.applied[sessionID] >= snap.Revision {
		return nil
	}
	if err := c.LCM.ApplySCMObservation(ctx, sessionID, observer.FactsFromSnapshot(snap)); err != nil {
		return err
	}
	if snap.Revision > c.applied[sessionID] {
		c.applied[sessionID] = snap.Revision
	}
	return nil
}
