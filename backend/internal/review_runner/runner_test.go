package reviewrunner

import (
	"context"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	reviewsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/review"
)

// fakeAgent is a minimal ports.Agent that records the launch config and returns
// a canned argv.
type fakeAgent struct{ gotLaunch ports.LaunchConfig }

func (f *fakeAgent) GetConfigSpec(context.Context) (ports.ConfigSpec, error) {
	return ports.ConfigSpec{}, nil
}
func (f *fakeAgent) GetLaunchCommand(_ context.Context, cfg ports.LaunchConfig) ([]string, error) {
	f.gotLaunch = cfg
	return []string{"review-agent", "--go"}, nil
}
func (f *fakeAgent) GetPromptDeliveryStrategy(context.Context, ports.LaunchConfig) (ports.PromptDeliveryStrategy, error) {
	return ports.PromptDeliveryInCommand, nil
}
func (f *fakeAgent) GetAgentHooks(context.Context, ports.WorkspaceHookConfig) error { return nil }
func (f *fakeAgent) GetRestoreCommand(context.Context, ports.RestoreConfig) ([]string, bool, error) {
	return nil, false, nil
}
func (f *fakeAgent) SessionInfo(context.Context, ports.SessionRef) (ports.SessionInfo, bool, error) {
	return ports.SessionInfo{}, false, nil
}

type fakeResolver struct {
	agent ports.Agent
	ok    bool
}

func (f fakeResolver) Agent(domain.AgentHarness) (ports.Agent, bool) { return f.agent, f.ok }

type fakeRuntime struct {
	cfg     ports.RuntimeConfig
	created bool
}

func (f *fakeRuntime) Create(_ context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	f.created = true
	f.cfg = cfg
	return ports.RuntimeHandle{ID: "h1"}, nil
}
func (f *fakeRuntime) Destroy(context.Context, ports.RuntimeHandle) error { return nil }
func (f *fakeRuntime) IsAlive(context.Context, ports.RuntimeHandle) (bool, error) {
	return true, nil
}

func TestRunLaunchesReviewerOverWorkerWorktree(t *testing.T) {
	agent := &fakeAgent{}
	rt := &fakeRuntime{}
	r := New(fakeResolver{agent: agent, ok: true}, rt)

	err := r.Run(context.Background(), reviewsvc.RunSpec{
		WorkerID:      "mer-1",
		Harness:       domain.HarnessCodex,
		WorkspacePath: "/ws/mer-1",
		PRURL:         "https://github.com/o/r/pull/1",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rt.created || rt.cfg.WorkspacePath != "/ws/mer-1" {
		t.Fatalf("runtime cfg = %+v created=%v", rt.cfg, rt.created)
	}
	if len(rt.cfg.Argv) == 0 || rt.cfg.Argv[0] != "review-agent" {
		t.Fatalf("argv = %v", rt.cfg.Argv)
	}
	if rt.cfg.Env["AO_REVIEW_WORKER"] != "mer-1" {
		t.Fatalf("env = %v, want AO_REVIEW_WORKER=mer-1", rt.cfg.Env)
	}
	// The launch prompt names the PR and the submit step.
	if !strings.Contains(agent.gotLaunch.Prompt, "pull/1") || !strings.Contains(agent.gotLaunch.Prompt, "ao review submit") {
		t.Fatalf("prompt missing PR/submit reference: %q", agent.gotLaunch.Prompt)
	}
}

func TestRunErrorsWhenNoAdapter(t *testing.T) {
	r := New(fakeResolver{ok: false}, &fakeRuntime{})
	err := r.Run(context.Background(), reviewsvc.RunSpec{Harness: "nope", WorkspacePath: "/ws"})
	if err == nil || !strings.Contains(err.Error(), "no agent adapter") {
		t.Fatalf("err = %v, want no-adapter error", err)
	}
}
