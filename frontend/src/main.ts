import { app, BrowserWindow, dialog, ipcMain, type MessageBoxOptions } from "electron";

type SessionStatus = "running" | "needs input" | "stuck" | "done" | "stopped" | "failed";
type AgentProvider = "codex" | "claude-code" | "opencode";
type LaunchMode = "local" | "worktree";

type WorkerSession = {
  id: string;
  workspaceId: string;
  title: string;
  provider: AgentProvider;
  status: SessionStatus;
  mode: LaunchMode;
  branch: string;
  elapsed: string;
};

type Workspace = {
  id: string;
  name: string;
  path: string;
  pinned?: boolean;
  workers: WorkerSession[];
};

const workspaces: Workspace[] = [
  {
    id: "vinesight-web",
    name: "vinesight-web",
    path: "~/vinesight-web",
    pinned: true,
    workers: [
      {
        id: "web-codex-18",
        workspaceId: "vinesight-web",
        title: "Review consultant dashboard PR",
        provider: "codex",
        status: "running",
        mode: "worktree",
        branch: "ao/review-consultant-dashboard",
        elapsed: "18m",
      },
      {
        id: "web-claude-07",
        workspaceId: "vinesight-web",
        title: "Fix login route copy",
        provider: "claude-code",
        status: "needs input",
        mode: "local",
        branch: "feature/consultant-dashboard",
        elapsed: "42m",
      },
    ],
  },
  {
    id: "agent-orchestrator",
    name: "agent-orchestrator",
    path: "~/agent-orchestrator",
    workers: [
      {
        id: "ao-open-03",
        workspaceId: "agent-orchestrator",
        title: "Prototype native PTY runtime",
        provider: "opencode",
        status: "stopped",
        mode: "worktree",
        branch: "ao/native-pty",
        elapsed: "1h 12m",
      },
    ],
  },
  {
    id: "ao-desktop",
    name: "agent-orchestrator-1",
    path: "~/agent-orchestrator-1",
    pinned: true,
    workers: [],
  },
];

function escapeHTML(value: string): string {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

// Real Lucide icon path data (ISC licensed, lucide-static v1.17.0). Inlined as
// raw <svg> inner markup because the renderer runs from a data: URL with no
// bundler. Rendered through icon() so stroke width and sizing stay uniform.
const ICONS: Record<string, string> = {
  "panel-left": '<rect width="18" height="18" x="3" y="3" rx="2"/><path d="M9 3v18"/><path d="m16 15-3-3 3-3"/>',
  folder:
    '<path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/>',
  "chevron-right": '<path d="m9 18 6-6-6-6"/>',
  search: '<path d="m21 21-4.34-4.34"/><circle cx="11" cy="11" r="8"/>',
  plus: '<path d="M5 12h14"/><path d="M12 5v14"/>',
  settings: '<path d="M14 17H5"/><path d="M19 7h-9"/><circle cx="17" cy="17" r="3"/><circle cx="7" cy="7" r="3"/>',
  square: '<rect width="18" height="18" x="3" y="3" rx="2"/>',
  x: '<path d="M18 6 6 18"/><path d="m6 6 12 12"/>',
  terminal: '<path d="m7 11 2-2-2-2"/><path d="M11 13h4"/><rect width="18" height="18" x="3" y="3" rx="2" ry="2"/>',
  command: '<path d="M15 6v12a3 3 0 1 0 3-3H6a3 3 0 1 0 3 3V6a3 3 0 1 0-3 3h12a3 3 0 1 0-3-3"/>',
  restart: '<path d="M21 12a9 9 0 1 1-9-9c2.52 0 4.93 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/>',
};

function icon(name: string, size = 16): string {
  return (
    `<svg class="ic" width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" ` +
    `stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" ` +
    `aria-hidden="true">${ICONS[name] ?? ""}</svg>`
  );
}

function workerRowHTML(worker: WorkerSession): string {
  return (
    `<button class="worker-row pressable" data-session-id="${escapeHTML(worker.id)}">` +
    `<span class="worker-dot status-${statusClass(worker.status)}"></span>` +
    `<span class="worker-title">${escapeHTML(worker.title)}</span>` +
    `<span class="worker-trailing status-${statusClass(worker.status)}">${sessionTrailingLabel(worker)}</span>` +
    `</button>`
  );
}

function buildAppHTML(): string {
  const workspaceRows = workspaces
    .map((workspace) => {
      const activeWorkers = workspace.workers.filter((worker) => worker.status !== "stopped");
      const historyCount = workspace.workers.length - activeWorkers.length;
      const workers = activeWorkers.map((worker) => workerRowHTML(worker)).join("");

      return `
        <section class="workspace-group" data-workspace-id="${escapeHTML(workspace.id)}">
          <div class="workspace-row pressable">
            <button class="workspace-disclosure" aria-label="Toggle ${escapeHTML(workspace.name)}">
              <span class="folder-icon">${icon("folder", 15)}</span>
              <span class="chevron-icon">${icon("chevron-right", 16)}</span>
            </button>
            <span class="workspace-name">${escapeHTML(workspace.name)}</span>
            <span class="workspace-actions">
              <span class="workspace-count">${activeWorkers.length}</span>
              <button class="workspace-new pressable" data-new-task="${escapeHTML(workspace.id)}" title="New task" aria-label="New task in ${escapeHTML(workspace.name)}">${icon("plus", 15)}</button>
            </span>
          </div>
          <div class="worker-list">
            ${workers || `<div class="worker-empty">No active workers</div>`}
            ${
              historyCount > 0
                ? `<button class="history-row pressable">${historyCount} stopped session${historyCount === 1 ? "" : "s"}</button>`
                : ""
            }
          </div>
        </section>`;
    })
    .join("");

  return `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>Agent Orchestrator</title>
    <style>${appCSS()}</style>
  </head>
  <body>
    <div class="app-shell">
      <aside class="sidebar">
        <div class="sidebar-space">
          <button class="sidebar-toggle pressable" title="Toggle sidebar" aria-label="Toggle sidebar">${icon("panel-left", 17)}</button>
        </div>
        <div class="pinned-list">
          <button class="orchestrator-row pressable is-active" id="show-orchestrator">
            <span class="row-icon">${icon("terminal", 16)}</span>
            <span class="orchestrator-title">Orchestrator</span>
            <span class="orchestrator-trailing"><span class="live-dot"></span>Live</span>
          </button>
        </div>
        <div class="sidebar-section-title">
          <span>Projects</span>
          <span class="sidebar-sort">Created</span>
        </div>
        <div class="workspace-list" id="workspace-list">${workspaceRows}</div>
        <div class="sidebar-footer">
          <button class="footer-menu-button pressable" id="sidebar-search-trigger">
            <span class="footer-menu-main">${icon("search", 16)}<span>Search</span></span>
            <span class="keycap">⌘K</span>
          </button>
          <button class="footer-menu-button pressable" id="sidebar-new-task">
            <span class="footer-menu-main">${icon("plus", 16)}<span>New task</span></span>
            <span class="keycap">⌘N</span>
          </button>
          <button class="footer-menu-button pressable" id="open-settings">
            <span class="footer-menu-main">${icon("settings", 16)}<span>Settings</span></span>
          </button>
        </div>
        <div class="sidebar-bottom">
          <button class="feedback-button pressable">Give feedback</button>
          <span class="daemon-pill"><span class="daemon-dot"></span>Ready</span>
        </div>
      </aside>
      <main class="main">
        <header class="topbar">
          <div class="view-title" id="view-title">
            <span class="view-kicker">Global</span>
            <h1>Orchestrator</h1>
          </div>
          <div class="topbar-actions">
            <button class="ghost-button pressable" id="command-palette">${icon("command", 14)}<span>Command</span><span class="keycap">K</span></button>
            <button class="ghost-button pressable" id="restart-orchestrator">${icon("restart", 14)}<span>Restart</span></button>
            <button class="primary-button pressable" id="top-new-task">${icon("plus", 15)}<span>New task</span></button>
          </div>
        </header>
        <section class="canvas">
          <div class="terminal-card" id="terminal-card">
            <div class="terminal-header">
              <div class="terminal-meta">
                <span class="row-icon">${icon("terminal", 15)}</span>
                <span class="terminal-label" id="terminal-label">ao-orchestrator</span>
                <span class="terminal-state" id="terminal-state">Codex</span>
                <span class="terminal-state" id="terminal-workers">2 active workers</span>
              </div>
              <button class="terminal-stop pressable" id="stop-current" title="Stop session" aria-label="Stop session">${icon("square", 14)}</button>
            </div>
            <pre class="terminal" id="terminal"></pre>
          </div>
        </section>
      </main>
    </div>
    <dialog class="modal" id="new-task-modal">
      <form method="dialog" class="modal-panel">
        <header class="modal-header">
          <div>
            <h2>New worker session</h2>
            <p id="modal-workspace">Select a workspace</p>
          </div>
          <button class="icon-button pressable" value="cancel" aria-label="Close">${icon("x", 16)}</button>
        </header>
        <label class="field">
          <span>Agent</span>
          <select id="task-agent">
            <option value="codex">Codex</option>
            <option value="claude-code">Claude Code</option>
            <option value="opencode">OpenCode</option>
          </select>
        </label>
        <div class="segmented" id="task-mode">
          <button type="button" class="pressable is-selected" data-mode="local">Work locally</button>
          <button type="button" class="pressable" data-mode="worktree">New worktree</button>
        </div>
        <label class="field">
          <span id="branch-label">Branch</span>
          <select id="task-branch">
            <option>main</option>
            <option>feature/consultant-dashboard</option>
            <option>codex/frontend-shell</option>
          </select>
        </label>
        <label class="field">
          <span>Task prompt</span>
          <textarea id="task-prompt" rows="5" placeholder="Describe the task, or leave empty to open a blank agent terminal"></textarea>
        </label>
        <footer class="modal-actions">
          <button class="ghost-button pressable" value="cancel">Cancel</button>
          <button class="primary-button pressable" id="create-worker" value="default">Start worker</button>
        </footer>
      </form>
    </dialog>
    <dialog class="modal" id="settings-modal">
      <form method="dialog" class="modal-panel settings-panel">
        <header class="modal-header">
          <div>
            <h2>Settings</h2>
            <p>Provider defaults, CLI paths, models, and diagnostics.</p>
          </div>
          <button class="icon-button pressable" value="cancel" aria-label="Close">${icon("x", 16)}</button>
        </header>
        <div class="settings-grid">
          <section>
            <h3>Defaults</h3>
            <label class="field"><span>Orchestrator agent</span><select><option>Codex</option><option>Claude Code</option><option>OpenCode</option></select></label>
            <label class="field"><span>Worker agent</span><select><option>Codex</option><option>Claude Code</option><option>OpenCode</option></select></label>
          </section>
          <section>
            <h3>Providers</h3>
            <div class="provider-row"><span>Codex</span><strong>Ready</strong></div>
            <div class="provider-row"><span>Claude Code</span><strong>Ready</strong></div>
            <div class="provider-row is-muted"><span>OpenCode</span><strong>Path override</strong></div>
          </section>
          <section>
            <h3>Diagnostics</h3>
            <button type="button" class="ghost-button pressable">View daemon logs</button>
            <button type="button" class="ghost-button pressable">Run provider checks</button>
          </section>
        </div>
      </form>
    </dialog>
    <script>${appJS()}</script>
  </body>
</html>`;
}

function statusClass(status: SessionStatus): string {
  return status.replaceAll(" ", "-");
}

function sessionTrailingLabel(worker: WorkerSession): string {
  switch (worker.status) {
    case "running":
      return escapeHTML(worker.elapsed);
    case "needs input":
      return "Needs input";
    case "stuck":
      return "Stuck";
    case "failed":
      return "Failed";
    case "done":
      return "Done";
    case "stopped":
      return "Stopped";
    default:
      return escapeHTML(worker.status);
  }
}

function appCSS(): string {
  return `
:root {
  color-scheme: light;
  --bg: #fafafa;
  --panel: #ffffff;
  --sidebar: #f4f4f5;
  --sidebar-hover: #ececee;
  --sidebar-active: #e4e4e7;
  --sidebar-strong: #dcdce0;
  --text: #18181b;
  --muted: #51525c;
  --faint: #a1a1aa;
  --line: #e6e6e9;
  --line-strong: #d6d6db;
  --accent: #2f6bf2;
  --focus: rgba(47, 107, 242, 0.45);
  --green: #16a34a;
  --amber: #b45309;
  --red: #dc2626;
  --terminal: #0c0d10;
  --terminal-2: #131419;
  --terminal-line: #23242e;
  --terminal-fg: #e4e4e7;
  --terminal-faint: #8b8d99;
  --r-sm: 6px;
  --r-md: 8px;
  --r-lg: 12px;
  --ease-out: cubic-bezier(0.23, 1, 0.32, 1);
  --ease: cubic-bezier(0.25, 0.1, 0.25, 1);
  font-family: -apple-system, BlinkMacSystemFont, "SF Pro Text", "Helvetica Neue", Arial, sans-serif;
  font-size: 14px;
  -webkit-font-smoothing: antialiased;
  text-rendering: optimizeLegibility;
}
* { box-sizing: border-box; }
body { margin: 0; min-height: 100vh; overflow: hidden; background: var(--bg); color: var(--text); }
button, input, select, textarea { font: inherit; color: inherit; }
button { cursor: pointer; }
.ic { display: block; flex: 0 0 auto; }
button:focus-visible, input:focus-visible, select:focus-visible, textarea:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 2px;
}
.pressable {
  transition:
    transform 140ms var(--ease-out),
    background-color 140ms var(--ease),
    border-color 140ms var(--ease),
    color 140ms var(--ease),
    opacity 140ms var(--ease);
}
.pressable:active { transform: scale(0.98); }

.app-shell { display: grid; grid-template-columns: 256px 1fr; height: 100vh; }

/* Sidebar */
.sidebar { display: flex; flex-direction: column; min-width: 0; background: var(--sidebar); color: var(--muted); border-right: 1px solid var(--line); }
.sidebar-space { display: flex; align-items: center; justify-content: flex-end; height: 44px; padding: 0 10px; -webkit-app-region: drag; }
.sidebar-toggle {
  display: inline-grid; place-items: center;
  width: 28px; height: 28px;
  border: 0; border-radius: var(--r-md);
  background: transparent; color: var(--muted);
  -webkit-app-region: no-drag;
}
.pinned-list { display: grid; grid-template-columns: minmax(0, 1fr); gap: 1px; padding: 4px 10px 8px; }
.orchestrator-row,
.workspace-row,
.worker-row,
.history-row,
.footer-menu-button {
  display: flex; align-items: center;
  min-height: 32px; width: 100%;
  border: 0; border-radius: var(--r-md);
  background: transparent; color: var(--muted);
  font-size: 13.5px; font-weight: 450;
  text-align: left;
}
.row-icon { display: inline-flex; align-items: center; color: var(--faint); }
.orchestrator-row { justify-content: flex-start; gap: 9px; padding: 0 10px; }
.orchestrator-title { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.orchestrator-row.is-active,
.workspace-row.is-active,
.worker-row.is-active,
.footer-menu-button.is-active { background: var(--sidebar-active); color: var(--text); }
.orchestrator-row.is-active .row-icon { color: var(--text); }
.orchestrator-trailing { display: inline-flex; align-items: center; gap: 6px; flex: 0 0 auto; color: var(--faint); font-size: 11px; }
.live-dot { width: 6px; height: 6px; border-radius: 999px; background: var(--green); }
.sidebar-section-title { display: flex; align-items: center; justify-content: space-between; height: 30px; padding: 0 14px 0 16px; color: var(--faint); font-size: 11px; font-weight: 600; letter-spacing: 0.03em; text-transform: uppercase; }
.sidebar-sort { font-weight: 500; text-transform: none; letter-spacing: 0; }
.workspace-list { flex: 1; min-height: 0; overflow-y: auto; padding: 0 10px 8px; }
.workspace-group { display: grid; grid-template-columns: minmax(0, 1fr); gap: 1px; margin-bottom: 2px; }
.workspace-row { justify-content: flex-start; gap: 2px; padding: 0 4px 0 4px; }
.workspace-disclosure {
  position: relative;
  display: inline-grid; place-items: center;
  flex: 0 0 auto; width: 26px; height: 26px;
  border: 0; border-radius: var(--r-sm);
  background: transparent; color: var(--muted);
}
.folder-icon,
.chevron-icon { position: absolute; display: inline-flex; transition: opacity 150ms var(--ease); }
.folder-icon { opacity: 1; }
.chevron-icon { opacity: 0; }
.workspace-name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--text); font-weight: 500; }
.workspace-actions { display: inline-flex; align-items: center; gap: 2px; flex: 0 0 auto; }
.workspace-count { min-width: 16px; text-align: center; color: var(--faint); font-size: 11px; font-variant-numeric: tabular-nums; }
.workspace-new {
  display: inline-grid; place-items: center;
  width: 24px; height: 24px;
  border: 0; border-radius: var(--r-sm);
  background: transparent; color: var(--muted);
  opacity: 0;
}
.worker-list { display: grid; grid-template-columns: minmax(0, 1fr); gap: 1px; }
.worker-row { justify-content: flex-start; gap: 8px; min-width: 0; padding: 0 8px 0 14px; }
.worker-dot { flex: 0 0 auto; width: 6px; height: 6px; border-radius: 999px; background: var(--faint); }
.worker-dot.status-running { background: var(--green); }
.worker-dot.status-needs-input,
.worker-dot.status-stuck { background: var(--amber); }
.worker-dot.status-failed { background: var(--red); }
.worker-title { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.worker-row.is-active .worker-title { color: var(--text); }
.worker-trailing { flex: 0 0 auto; margin-left: 8px; color: var(--faint); font-size: 11px; font-variant-numeric: tabular-nums; }
.worker-trailing.status-needs-input,
.worker-trailing.status-stuck { color: var(--amber); }
.worker-trailing.status-failed { color: var(--red); }
.worker-empty,
.history-row { display: flex; align-items: center; height: 30px; padding: 0 8px 0 14px; color: var(--faint); font-size: 12.5px; }

.sidebar-footer { display: grid; grid-template-columns: minmax(0, 1fr); gap: 1px; margin-top: auto; padding: 8px 10px; border-top: 1px solid var(--line); }
.footer-menu-button { justify-content: space-between; gap: 8px; padding: 0 10px; }
.footer-menu-main { display: inline-flex; min-width: 0; align-items: center; gap: 9px; }
.footer-menu-main .ic { color: var(--faint); }
.footer-menu-button:hover .ic,
.footer-menu-button.is-active .ic { color: var(--text); }
.keycap { display: inline-flex; align-items: center; justify-content: center; gap: 1px; min-width: 20px; height: 18px; padding: 0 5px; border-radius: var(--r-sm); background: var(--sidebar-strong); color: var(--muted); font-size: 11px; font-weight: 600; }
.sidebar-bottom { display: flex; align-items: center; justify-content: space-between; gap: 8px; min-height: 42px; padding: 8px 14px; border-top: 1px solid var(--line); }
.feedback-button { min-width: 0; height: 24px; border: 0; border-radius: var(--r-md); background: transparent; padding: 0 8px; margin-left: -8px; color: var(--muted); font-size: 12.5px; text-align: left; }
.daemon-pill { display: inline-flex; align-items: center; gap: 6px; color: var(--faint); font-size: 11.5px; }
.daemon-dot { width: 6px; height: 6px; border-radius: 999px; background: var(--green); }

/* Main */
.main { display: flex; flex-direction: column; min-width: 0; background: var(--bg); }
.topbar { height: 56px; display: flex; align-items: center; justify-content: space-between; gap: 20px; padding: 0 16px; border-bottom: 1px solid var(--line); -webkit-app-region: drag; }
.view-title { min-width: 0; }
.view-kicker { display: block; color: var(--faint); font-size: 10.5px; font-weight: 600; letter-spacing: 0.07em; text-transform: uppercase; }
h1 { margin: 1px 0 0; font-size: 18px; line-height: 1.2; font-weight: 600; letter-spacing: -0.01em; }
.topbar-actions { display: inline-flex; align-items: center; gap: 8px; -webkit-app-region: no-drag; }
.ghost-button, .primary-button {
  display: inline-flex; align-items: center; gap: 6px;
  height: 30px; padding: 0 11px;
  border: 1px solid var(--line-strong); border-radius: var(--r-md);
  background: var(--panel); color: var(--text);
  font-size: 12.5px; font-weight: 500;
}
.ghost-button .ic { color: var(--muted); }
.ghost-button .keycap { background: var(--sidebar-active); }
.primary-button { border-color: var(--text); background: var(--text); color: #fafafa; font-weight: 550; }
.primary-button .ic { color: #fafafa; }
.icon-button {
  display: inline-grid; place-items: center;
  width: 30px; height: 30px; padding: 0;
  border: 1px solid var(--line-strong); border-radius: var(--r-md);
  background: var(--panel); color: var(--muted);
}
.canvas { flex: 1; min-height: 0; padding: 12px; }
.terminal-card { height: 100%; min-height: 420px; display: flex; flex-direction: column; overflow: hidden; border-radius: var(--r-lg); background: var(--terminal); box-shadow: 0 1px 2px rgba(15, 16, 22, 0.16), 0 16px 40px rgba(15, 16, 22, 0.14); }
.terminal-header { height: 38px; display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 0 10px 0 12px; border-bottom: 1px solid var(--terminal-line); background: var(--terminal-2); color: var(--terminal-fg); font-size: 12px; }
.terminal-meta { display: inline-flex; align-items: center; gap: 8px; min-width: 0; }
.terminal-meta .row-icon { color: var(--terminal-faint); }
.terminal-label { color: #f3f3f5; font-family: "SF Mono", ui-monospace, Menlo, Monaco, Consolas, monospace; font-weight: 600; }
.terminal-state { position: relative; padding-left: 9px; color: var(--terminal-faint); }
.terminal-state::before { content: ""; position: absolute; left: 0; top: 50%; width: 3px; height: 3px; margin-top: -1px; border-radius: 999px; background: var(--terminal-line); }
.terminal-stop { display: inline-grid; place-items: center; width: 26px; height: 26px; border: 0; border-radius: var(--r-sm); background: transparent; color: var(--terminal-faint); }
.terminal { flex: 1; margin: 0; padding: 16px 18px; overflow: auto; color: var(--terminal-fg); font-family: "SF Mono", ui-monospace, Menlo, Monaco, Consolas, monospace; font-size: 13px; line-height: 1.55; white-space: pre-wrap; }
.terminal .muted { color: var(--terminal-faint); }

/* Modals */
.modal { width: fit-content; max-width: calc(100vw - 40px); border: 0; border-radius: var(--r-lg); padding: 0; background: transparent; }
.modal::backdrop { background: rgba(9, 9, 11, 0.32); backdrop-filter: blur(3px); }
.modal-panel { display: grid; gap: 14px; width: min(560px, calc(100vw - 40px)); margin: 0; padding: 20px; border: 1px solid var(--line); border-radius: var(--r-lg); background: var(--panel); box-shadow: 0 24px 70px rgba(9, 9, 11, 0.22); }
.modal-header { display: flex; align-items: flex-start; justify-content: space-between; gap: 14px; }
.modal-header h2 { margin: 0; font-size: 16px; font-weight: 600; letter-spacing: -0.01em; }
.modal-header p { margin: 4px 0 0; color: var(--muted); font-size: 12.5px; }
.field { display: grid; gap: 6px; color: var(--muted); font-size: 11.5px; font-weight: 600; }
.field select, .field textarea {
  width: 100%;
  border: 1px solid var(--line-strong); border-radius: var(--r-md);
  background: var(--panel); padding: 8px 10px;
  color: var(--text); font-weight: 400; font-size: 13.5px;
  transition: border-color 140ms var(--ease), box-shadow 140ms var(--ease);
}
.field select:focus, .field textarea:focus {
  outline: none;
  border-color: var(--accent);
  box-shadow: 0 0 0 3px var(--focus);
}
.field textarea { resize: vertical; min-height: 110px; line-height: 1.5; }
.segmented { display: grid; grid-template-columns: 1fr 1fr; gap: 3px; padding: 3px; border: 1px solid var(--line); border-radius: var(--r-md); background: var(--sidebar); }
.segmented button { height: 30px; border: 0; border-radius: var(--r-sm); background: transparent; font-weight: 550; font-size: 12.5px; color: var(--muted); }
.segmented button.is-selected { background: var(--panel); color: var(--text); box-shadow: 0 1px 2px rgba(9, 9, 11, 0.1); }
.modal-actions { display: flex; justify-content: flex-end; gap: 8px; }
.settings-panel { width: min(840px, calc(100vw - 40px)); }
.settings-grid { display: grid; grid-template-columns: repeat(3, 1fr); gap: 20px; }
.settings-grid section { display: grid; gap: 8px; align-content: start; }
.settings-grid h3 { margin: 0 0 4px; font-size: 12px; font-weight: 600; color: var(--text); }
.provider-row { display: flex; align-items: center; justify-content: space-between; gap: 10px; min-height: 32px; border-bottom: 1px solid var(--line); font-size: 12.5px; color: var(--text); }
.provider-row strong { color: var(--green); font-size: 11px; font-weight: 600; }
.provider-row.is-muted strong { color: var(--amber); }

@media (hover: hover) and (pointer: fine) {
  .sidebar-toggle:hover,
  .orchestrator-row:hover,
  .workspace-row:hover,
  .worker-row:hover,
  .footer-menu-button:hover,
  .feedback-button:hover { background: var(--sidebar-hover); color: var(--text); }
  .workspace-row:hover .workspace-new { opacity: 1; }
  .workspace-new:hover { background: var(--sidebar-strong); color: var(--text); }
  .workspace-row:hover .folder-icon { opacity: 0; }
  .workspace-row:hover .chevron-icon { opacity: 1; }
  .ghost-button:hover, .icon-button:hover { background: var(--sidebar); border-color: var(--faint); }
  .primary-button:hover { background: #27272a; border-color: #27272a; }
  .terminal-stop:hover { background: var(--terminal-line); color: #f3f3f5; }
}
@media (prefers-reduced-motion: reduce) {
  .pressable,
  .folder-icon,
  .chevron-icon,
  .field select,
  .field textarea { transition-duration: 0ms; }
  .pressable:active { transform: none; }
}
@media (max-width: 900px) {
  .app-shell { grid-template-columns: 240px 1fr; }
  .settings-grid { grid-template-columns: 1fr; }
}
`;
}

function appJS(): string {
  return `
const ICONS = ${JSON.stringify(ICONS)};
function icon(name, size) {
  size = size || 16;
  return '<svg class="ic" width="' + size + '" height="' + size + '" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + (ICONS[name] || "") + '</svg>';
}
const workspaces = ${JSON.stringify(workspaces)};
let selectedWorkspaceId = workspaces[0]?.id ?? null;
let selectedMode = "orchestrator";
let selectedSessionId = null;
let taskMode = "local";

const terminal = document.getElementById("terminal");
const title = document.getElementById("view-title");
const label = document.getElementById("terminal-label");
const state = document.getElementById("terminal-state");
const modal = document.getElementById("new-task-modal");
const settings = document.getElementById("settings-modal");
const modalWorkspace = document.getElementById("modal-workspace");
const promptBox = document.getElementById("task-prompt");
const branchLabel = document.getElementById("branch-label");

function providerName(id) {
  return id === "claude-code" ? "Claude Code" : id === "opencode" ? "OpenCode" : "Codex";
}

function allWorkers() {
  return workspaces.flatMap((workspace) => workspace.workers.map((worker) => ({ ...worker, workspace })));
}

function statusClass(status) {
  return status.replaceAll(" ", "-");
}

function sessionTrailingLabel(worker) {
  switch (worker.status) {
    case "running": return escapeHTML(worker.elapsed);
    case "needs input": return "Needs input";
    case "stuck": return "Stuck";
    case "failed": return "Failed";
    case "done": return "Done";
    case "stopped": return "Stopped";
    default: return escapeHTML(worker.status);
  }
}

function escapeHTML(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

function workerRowMarkup(worker) {
  const active = selectedSessionId === worker.id ? " is-active" : "";
  return '<button class="worker-row pressable' + active + '" data-session-id="' + escapeHTML(worker.id) + '">' +
    '<span class="worker-dot status-' + statusClass(worker.status) + '"></span>' +
    '<span class="worker-title">' + escapeHTML(worker.title) + '</span>' +
    '<span class="worker-trailing status-' + statusClass(worker.status) + '">' + sessionTrailingLabel(worker) + '</span>' +
    '</button>';
}

function workspaceMarkup(workspace) {
  const activeWorkers = workspace.workers.filter((worker) => worker.status !== "stopped");
  const historyCount = workspace.workers.length - activeWorkers.length;
  const workers = activeWorkers.map(workerRowMarkup).join("");
  return '<section class="workspace-group" data-workspace-id="' + escapeHTML(workspace.id) + '">' +
    '<div class="workspace-row pressable">' +
    '<button class="workspace-disclosure" aria-label="Toggle ' + escapeHTML(workspace.name) + '">' +
    '<span class="folder-icon">' + icon("folder", 15) + '</span>' +
    '<span class="chevron-icon">' + icon("chevron-right", 16) + '</span>' +
    '</button>' +
    '<span class="workspace-name">' + escapeHTML(workspace.name) + '</span>' +
    '<span class="workspace-actions">' +
    '<span class="workspace-count">' + activeWorkers.length + '</span>' +
    '<button class="workspace-new pressable" data-new-task="' + escapeHTML(workspace.id) + '" title="New task" aria-label="New task in ' + escapeHTML(workspace.name) + '">' + icon("plus", 15) + '</button>' +
    '</span>' +
    '</div>' +
    '<div class="worker-list">' +
    (workers || '<div class="worker-empty">No active workers</div>') +
    (historyCount > 0 ? '<button class="history-row pressable">' + historyCount + ' stopped session' + (historyCount === 1 ? "" : "s") + '</button>' : '') +
    '</div>' +
    '</section>';
}

function attachSidebarHandlers() {
  document.querySelectorAll("[data-session-id]").forEach((button) => {
    button.addEventListener("click", () => showWorker(button.dataset.sessionId));
  });
  document.querySelectorAll("[data-new-task]").forEach((button) => {
    button.addEventListener("click", (event) => {
      event.stopPropagation();
      openNewTask(button.dataset.newTask);
    });
  });
}

function showOrchestrator() {
  selectedMode = "orchestrator";
  selectedSessionId = null;
  document.querySelectorAll(".is-active").forEach((node) => node.classList.remove("is-active"));
  document.getElementById("show-orchestrator").classList.add("is-active");
  title.innerHTML = '<span class="view-kicker">Global</span><h1>Orchestrator</h1>';
  label.textContent = "ao-orchestrator";
  state.textContent = "Codex";
  terminal.innerHTML = [
    '<span class="muted">Agent Orchestrator</span>',
    '',
    'orchestrator ready',
    'scope     all workspaces',
    'workers   2 active',
    '',
    'Type a high-level instruction to coordinate your workers.',
    '<span class="muted">The agent terminal attaches once the backend session is live.</span>'
  ].join("\\n");
}

function showWorker(sessionId) {
  const item = allWorkers().find((worker) => worker.id === sessionId);
  if (!item) return;
  selectedMode = "worker";
  selectedSessionId = sessionId;
  selectedWorkspaceId = item.workspace.id;
  document.querySelectorAll(".is-active").forEach((node) => node.classList.remove("is-active"));
  document.querySelector('[data-session-id="' + CSS.escape(sessionId) + '"]')?.classList.add("is-active");
  title.innerHTML = '<span class="view-kicker">' + escapeHTML(item.workspace.name) + '</span><h1>' + escapeHTML(item.title) + '</h1>';
  label.textContent = item.id;
  state.textContent = providerName(item.provider);
  terminal.innerHTML = [
    '<span class="muted">worker session</span>',
    '',
    'session   ' + escapeHTML(item.id),
    'provider  ' + providerName(item.provider),
    'workspace ' + escapeHTML(item.workspace.name),
    'mode      ' + item.mode,
    'branch    ' + escapeHTML(item.branch),
    '',
    item.status === "stopped"
      ? '<span class="muted">Session stopped. Metadata is retained, transcript is not.</span>'
      : '<span class="muted">Terminal connection pending backend mux.</span>'
  ].join("\\n");
}

function openNewTask(workspaceId) {
  selectedWorkspaceId = workspaceId ?? selectedWorkspaceId ?? workspaces[0]?.id;
  const workspace = workspaces.find((candidate) => candidate.id === selectedWorkspaceId);
  modalWorkspace.textContent = workspace ? workspace.name + "  " + workspace.path : "Select a workspace";
  promptBox.value = "";
  modal.showModal();
}

function createWorker() {
  const workspace = workspaces.find((candidate) => candidate.id === selectedWorkspaceId);
  if (!workspace) return;
  const agent = document.getElementById("task-agent").value;
  const branch = document.getElementById("task-branch").value;
  const prompt = promptBox.value.trim();
  const id = workspace.id.slice(0, 4) + "-" + agent.replace("-", "").slice(0, 5) + "-" + String(Date.now()).slice(-4);
  const worker = {
    id,
    workspaceId: workspace.id,
    title: prompt ? prompt.split(/[.!?\\n]/)[0].slice(0, 72) : "Manual " + providerName(agent) + " terminal",
    provider: agent,
    status: "running",
    mode: taskMode,
    branch,
    elapsed: "just now"
  };
  workspace.workers.unshift(worker);
  modal.close();
  renderWorkspaceList();
  showWorker(id);
}

function renderWorkspaceList() {
  const container = document.getElementById("workspace-list");
  container.innerHTML = workspaces.map(workspaceMarkup).join("");
  attachSidebarHandlers();
}

document.getElementById("show-orchestrator").addEventListener("click", showOrchestrator);
document.getElementById("sidebar-new-task").addEventListener("click", () => openNewTask(selectedWorkspaceId));
document.getElementById("sidebar-search-trigger").addEventListener("click", () => document.getElementById("command-palette").click());
document.getElementById("top-new-task").addEventListener("click", () => openNewTask(selectedWorkspaceId));
document.getElementById("open-settings").addEventListener("click", () => settings.showModal());
document.getElementById("command-palette").addEventListener("click", () => alert("Command palette target: switch sessions, open settings, create task, restart orchestrator."));
document.getElementById("restart-orchestrator").addEventListener("click", () => alert("Restart orchestrator will stop and start the global orchestrator session in the backend slice."));
document.getElementById("stop-current").addEventListener("click", () => alert(selectedMode === "orchestrator" ? "Stop orchestrator action" : "Stop worker " + selectedSessionId));
document.getElementById("create-worker").addEventListener("click", createWorker);
attachSidebarHandlers();
document.querySelectorAll("#task-mode button").forEach((button) => {
  button.addEventListener("click", () => {
    taskMode = button.dataset.mode;
    document.querySelectorAll("#task-mode button").forEach((item) => item.classList.remove("is-selected"));
    button.classList.add("is-selected");
    branchLabel.textContent = taskMode === "worktree" ? "Base branch" : "Branch";
  });
});
document.addEventListener("keydown", (event) => {
  if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
    event.preventDefault();
    document.getElementById("command-palette").click();
  }
  if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "n") {
    event.preventDefault();
    openNewTask(selectedWorkspaceId);
  }
});

showOrchestrator();
`;
}

function createWindow(): BrowserWindow {
  const window = new BrowserWindow({
    width: 1320,
    height: 860,
    minWidth: 980,
    minHeight: 620,
    title: "Agent Orchestrator",
    titleBarStyle: "hiddenInset",
    trafficLightPosition: { x: 14, y: 15 },
    backgroundColor: "#f4f4f5",
    webPreferences: {
      contextIsolation: true,
      nodeIntegration: false,
    },
  });

  void window.loadURL(`data:text/html;charset=utf-8,${encodeURIComponent(buildAppHTML())}`);
  return window;
}

async function confirmStopSessions(): Promise<boolean> {
  const focusedWindow = BrowserWindow.getFocusedWindow();
  const options: MessageBoxOptions = {
    type: "warning",
    buttons: ["Stop sessions and close", "Cancel"],
    defaultId: 0,
    cancelId: 1,
    title: "Stop active sessions?",
    message: "Closing Agent Orchestrator will stop the orchestrator and active worker sessions.",
    detail: "AO Desktop does not keep agent sessions running in the background.",
  } as const;
  const result = focusedWindow
    ? await dialog.showMessageBox(focusedWindow, options)
    : await dialog.showMessageBox(options);
  return result.response === 0;
}

let quitting = false;

app.whenReady().then(() => {
  ipcMain.handle("app:confirm-stop-sessions", confirmStopSessions);
  createWindow();

  app.on("activate", () => {
    if (BrowserWindow.getAllWindows().length === 0) {
      createWindow();
    }
  });
});

app.on("before-quit", async (event) => {
  if (quitting) return;
  event.preventDefault();
  if (await confirmStopSessions()) {
    quitting = true;
    app.quit();
  }
});

app.on("window-all-closed", () => {
  if (process.platform !== "darwin") {
    app.quit();
  }
});
