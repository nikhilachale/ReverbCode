package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/messenger/composite"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/messenger/inbox"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/messenger/panep"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/zellij"
	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// TestWiring_WriteFlowsToBroadcaster exercises the real boot path end to end:
// a lifecycle write -> sqlite -> DB trigger -> change_log -> CDC poller ->
// broadcaster, through the same cdc.Source implementation the daemon uses.
func TestWiring_WriteFlowsToBroadcaster(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	lcm := lifecycle.New(store, nil)

	bcast := cdc.NewBroadcaster()
	poller := cdc.NewPoller(store, bcast, cdc.PollerConfig{})
	if err := poller.SeekToHead(ctx); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []cdc.Event
	bcast.Subscribe(func(e cdc.Event) { mu.Lock(); got = append(got, e); mu.Unlock() })

	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "mer", Path: "/repo/mer"}); err != nil {
		t.Fatal(err)
	}
	rec, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "mer", Kind: domain.KindWorker,
		Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A real transition through the engine, which writes the row and fires the
	// activity_state/is_terminated CDC trigger.
	if err := lcm.ApplyActivitySignal(ctx, rec.ID, ports.ActivitySignal{Valid: true, State: domain.ActivityActive, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}

	if err := poller.Poll(ctx); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	var sawSession bool
	for _, e := range got {
		if e.SessionID == string(rec.ID) {
			sawSession = true
		}
	}
	if !sawSession {
		t.Fatalf("expected a change_log event for %s to reach the broadcaster, got %d events", rec.ID, len(got))
	}
}

// TestWiring_AgentResolverResolvesRealAdapters asserts buildAgentResolver wires a
// real registry-backed per-session resolver: each harness resolves to the
// matching registered adapter, an empty harness falls back to the AO_AGENT
// default, and an unknown harness misses.
func TestWiring_AgentResolverResolvesRealAdapters(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	resolver, err := buildAgentResolver("", log) // empty default → claude-code
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		harness domain.AgentHarness
		wantID  string
	}{
		{domain.HarnessClaudeCode, "claude-code"},
		{domain.HarnessCodex, "codex"},
		{"", config.DefaultAgent}, // empty harness falls back to the AO_AGENT default
	} {
		agent, ok := resolver.Agent(tc.harness)
		if !ok {
			t.Fatalf("resolver has no agent for harness %q", tc.harness)
		}
		described, ok := agent.(adapters.Adapter)
		if !ok {
			t.Fatalf("agent for harness %q is %T, not a registered adapters.Adapter", tc.harness, agent)
		}
		if got := described.Manifest().ID; got != tc.wantID {
			t.Fatalf("harness %q resolved to adapter %q, want %q", tc.harness, got, tc.wantID)
		}
	}
	if _, ok := resolver.Agent("definitely-not-an-agent"); ok {
		t.Fatal("unknown harness resolved to an agent; want a miss")
	}
}

// TestWiring_StartSessionBuildsSessionService asserts the daemon's startSession
// constructs a real controller-facing session service end to end (resolver +
// gitworktree workspace + session manager over the shared store/LCM), which is
// what gets mounted at httpd APIDeps.Sessions.
func TestWiring_StartSessionBuildsSessionService(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	lcm := lifecycle.New(store, nil)
	cfg := config.Config{DataDir: t.TempDir()}

	runtime := zellij.New(zellij.Options{})
	messenger := newSessionMessenger(store, runtime, log)
	svc, err := startSession(cfg, runtime, store, lcm, messenger, log)
	if err != nil {
		t.Fatalf("startSession: %v", err)
	}
	if svc == nil {
		t.Fatal("startSession returned nil session service")
	}
}

// TestWiring_SessionMessengerIsInboxThenPanepComposite asserts the daemon wires
// the agent messenger as a composite of inbox (primary, durable file write)
// then panep (secondary, live pane ping) — the ordering the "primary must
// succeed, secondaries are nudges" contract depends on. It also proves the
// messenger reaches the same store the SM reads: a Send through a row the store
// owns lands an inbox file under that row's workspace. This is the switch that
// replaced the old noopMessenger, which silently dropped every nudge.
func TestWiring_SessionMessengerIsInboxThenPanepComposite(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	runtime := zellij.New(zellij.Options{})
	messenger := newSessionMessenger(store, runtime, nil)

	comp, ok := messenger.(*composite.Messenger)
	if !ok {
		t.Fatalf("session messenger should be *composite.Messenger, got %T", messenger)
	}
	if len(comp.Inner) != 2 {
		t.Fatalf("composite should wrap exactly 2 inner messengers (inbox + panep), got %d", len(comp.Inner))
	}
	if _, ok := comp.Inner[0].(*inbox.Messenger); !ok {
		t.Errorf("composite Inner[0] should be *inbox.Messenger (primary), got %T", comp.Inner[0])
	}
	if _, ok := comp.Inner[1].(*panep.Messenger); !ok {
		t.Errorf("composite Inner[1] should be *panep.Messenger (secondary), got %T", comp.Inner[1])
	}

	// End-to-end: a session row in the shared store is reachable through the
	// messenger. A second store would surface as "session not found" here.
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: "/repo/p", RegisteredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	workspaceDir := t.TempDir()
	rec, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "p", Kind: domain.KindWorker,
		Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now()},
		Metadata: domain.SessionMetadata{WorkspacePath: workspaceDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	// panep will fail (no live zellij pane), but it is best-effort: Send must
	// still succeed because the inbox file write (primary) succeeded.
	if err := messenger.Send(ctx, rec.ID, "hello agent"); err != nil {
		t.Fatalf("messenger.Send through shared store lookup: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(workspaceDir, ".ao", "inbox"))
	if err != nil {
		t.Fatalf("inbox dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 inbox file, got %d", len(entries))
	}
}

// TestProjectRepoResolver_ResolvesRegisteredProject asserts the DB-backed repo
// resolver turns a registered project into its on-disk repo path (so spawns
// materialise a worktree), and fails loudly for an unregistered project.
func TestProjectRepoResolver_ResolvesRegisteredProject(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "mer", Path: "/repo/mer", RegisteredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	r := projectRepoResolver{store: store}
	got, err := r.RepoPath("mer")
	if err != nil {
		t.Fatalf("RepoPath(mer): %v", err)
	}
	if got != "/repo/mer" {
		t.Fatalf("RepoPath(mer) = %q, want /repo/mer", got)
	}
	_, err = r.RepoPath("nope")
	if err == nil {
		t.Fatal("expected an error for an unregistered project")
	}
	// Guard the sentinel wrapping so the HTTP 400 mapping can't silently regress.
	if !errors.Is(err, sessionmanager.ErrProjectNotResolvable) {
		t.Fatalf("unregistered-project error should wrap ErrProjectNotResolvable, got %v", err)
	}
}

// TestDaemonZellijSocketDir_LeavesBudgetForSessionNames guards the fix for the
// zellij "session name must be less than 0 characters" spawn failure: the
// daemon's socket dir must be short enough that a max-length (48-char) session
// name still fits the ~103-byte unix-domain-socket-path budget. zellij's long
// $TMPDIR default (the bug) would fail this.
func TestDaemonZellijSocketDir_LeavesBudgetForSessionNames(t *testing.T) {
	dir := zellij.DefaultSocketDir()
	if dir == "" {
		t.Skip("zellij not used on this platform")
	}
	const (
		unixSocketPathMax = 103 // sun_path budget zellij enforces on macOS
		zellijOverhead    = 24  // zellij's version subdir + separators (generous)
		maxSessionName    = 48  // zellijSessionName's cap
	)
	if budget := unixSocketPathMax - len(dir) - zellijOverhead; budget < maxSessionName {
		t.Fatalf("zellij socket dir %q too long: %d bytes left for the session name, need >= %d", dir, budget, maxSessionName)
	}
}
