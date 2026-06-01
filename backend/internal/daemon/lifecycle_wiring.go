package daemon

import (
	"context"
	"log/slog"

	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/reaper"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// lifecycleStack owns the running lifecycle reducer and runtime reaper. The
// reducer writes session facts; the reaper probes live runtimes and reports
// observations back through it.
type lifecycleStack struct {
	LCM        *lifecycle.Manager
	Store      *sqlite.Store
	reaperDone <-chan struct{}
}

// startLifecycle constructs the Lifecycle Manager over the store and starts the
// reaper. The goroutine stops when ctx is cancelled; Stop waits for it to drain.
func startLifecycle(ctx context.Context, store *sqlite.Store, runtime ports.Runtime, logger *slog.Logger) *lifecycleStack {
	lcm := lifecycle.New(store, nil)
	rp := reaper.New(lcm, store, runtime, reaper.Config{Logger: logger})
	return &lifecycleStack{LCM: lcm, Store: store, reaperDone: rp.Start(ctx)}
}

// Stop waits for the reaper goroutine to exit. The caller must cancel the ctx
// passed to startLifecycle before calling Stop.
func (l *lifecycleStack) Stop() { <-l.reaperDone }
