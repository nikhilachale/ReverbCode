package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// spawnRequestTimeout bounds a single POST /api/v1/sessions call. It is
// deliberately longer than DefaultDeps.HTTPClient.Timeout (which is sized for
// fast probes like /healthz and /shutdown) because spawn synchronously creates
// a worktree, launches a zellij pane, and starts the agent — that can comfortably
// exceed 2 s on a cold cache. 90 s buys headroom over the server's
// config.DefaultRequestTimeout (60 s) without hanging the CLI forever on a
// truly stuck daemon.
const spawnRequestTimeout = 90 * time.Second

type spawnOptions struct {
	project string
	prompt  string
	agent   string
}

func newSpawnCommand(ctx *commandContext) *cobra.Command {
	var opts spawnOptions
	cmd := &cobra.Command{
		Use:   "spawn",
		Short: "Spawn a new agent session",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctx.spawnSession(cmd.Context(), cmd.OutOrStdout(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.prompt, "prompt", "", "Initial prompt for the agent")
	cmd.Flags().StringVar(&opts.project, "project", "", "Project id")
	cmd.Flags().StringVar(&opts.agent, "agent", "claude-code", "Agent plugin")
	return cmd
}

type spawnAPIRequest struct {
	ProjectID string `json:"projectId"`
	Prompt    string `json:"prompt"`
	Agent     string `json:"agent,omitempty"`
}

type spawnAPIResponse struct {
	SessionID     string `json:"sessionId"`
	WorkspacePath string `json:"workspacePath"`
	RuntimeHandle string `json:"runtimeHandle"`
}

type apiError struct {
	Kind    string `json:"error"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (c *commandContext) spawnSession(ctx context.Context, out io.Writer, opts spawnOptions) error {
	prompt := strings.TrimSpace(opts.prompt)
	if prompt == "" {
		return usageError{errors.New("usage: --prompt is required")}
	}
	project := strings.TrimSpace(opts.project)
	if project == "" {
		return usageError{errors.New("usage: --project is required")}
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	info, err := runfile.Read(cfg.RunFilePath)
	if err != nil {
		return fmt.Errorf("read run-file: %w", err)
	}
	if info == nil {
		return errors.New("AO daemon is not running; start it with `ao start`")
	}

	payload := spawnAPIRequest{
		ProjectID: project,
		Prompt:    prompt,
		Agent:     opts.agent,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	url := fmt.Sprintf("http://%s:%d/api/v1/sessions", config.LoopbackHost, info.Port)

	reqCtx, cancel := context.WithTimeout(ctx, spawnRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// Use a dedicated client (no client-level timeout) so the deadline is
	// driven solely by reqCtx. The shared deps.HTTPClient is sized for
	// short-lived probes; reusing it here would preempt spawn long before
	// the daemon could finish provisioning.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("daemon request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var ok spawnAPIResponse
		if err := json.Unmarshal(respBody, &ok); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		_, err := fmt.Fprintf(out, "Spawned session %s in %s\nAttach: zellij attach %s\n",
			ok.SessionID, ok.WorkspacePath, ok.RuntimeHandle)
		return err
	}

	// Non-2xx: surface the server's error envelope when present, otherwise the
	// raw status. Both 4xx and 5xx exit 1; usage errors (which exit 2) come from
	// flag validation above.
	var apiErr apiError
	if jerr := json.Unmarshal(respBody, &apiErr); jerr == nil && apiErr.Kind != "" {
		return fmt.Errorf("%s: %s", apiErr.Kind, apiErr.Message)
	}
	return fmt.Errorf("daemon returned HTTP %d", resp.StatusCode)
}
