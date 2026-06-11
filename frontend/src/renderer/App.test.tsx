import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, test, vi } from "vitest";
import { App } from "./App";
import { TooltipProvider } from "./components/ui/tooltip";
import { useUiStore } from "./stores/ui-store";

const { postMock, patchMock, mockData } = vi.hoisted(() => ({
	postMock: vi.fn(),
	patchMock: vi.fn(),
	mockData: {
		projectsError: undefined as Error | undefined,
		projects: [] as { id: string; name: string; path: string; sessionPrefix: string }[],
		sessions: [] as {
			id: string;
			projectId: string;
			displayName?: string;
			harness?: string;
			status: string;
			isTerminated: boolean;
			isArchived?: boolean;
			updatedAt: string;
		}[],
	},
}));

vi.mock("./lib/api-client", () => ({
	getApiBaseUrl: () => "http://127.0.0.1:3001",
	setApiBaseUrl: () => undefined,
	subscribeApiBaseUrl: () => () => undefined,
	apiClient: {
		GET: vi.fn(async (url: string) => {
			if (url === "/api/v1/projects") {
				if (mockData.projectsError) return { data: undefined, error: mockData.projectsError };
				return { data: { projects: mockData.projects }, error: undefined };
			}
			if (url === "/api/v1/sessions") {
				return { data: { sessions: mockData.sessions }, error: undefined };
			}
			return { data: undefined, error: new Error(`unexpected GET ${url}`) };
		}),
		POST: postMock,
		PATCH: patchMock,
	},
}));

vi.mock("./components/TerminalPane", () => ({
	TerminalPane: () => <div>Terminal scaffold</div>,
}));

function renderApp() {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	// TooltipProvider mirrors routes/__root.tsx, which wraps App in production.
	return render(
		<QueryClientProvider client={queryClient}>
			<TooltipProvider>
				<App />
			</TooltipProvider>
		</QueryClientProvider>,
	);
}

beforeEach(() => {
	postMock.mockReset();
	patchMock.mockReset();
	mockData.projectsError = undefined;
	mockData.projects = [];
	mockData.sessions = [];
	window.localStorage.clear();
	useUiStore.setState({
		view: "orchestrator",
		workbenchTab: "changes",
		isSidebarOpen: true,
		selectedSessionId: null,
		selectedWorkspaceId: null,
		theme: "dark",
	});
});

test("renders the orchestrator anchor and empty state", async () => {
	renderApp();

	expect(await screen.findByRole("button", { name: "Orchestrator" })).toBeInTheDocument();
	expect(await screen.findByText("No projects yet.")).toBeInTheDocument();
});

test("surfaces project load failures instead of the empty state", async () => {
	mockData.projectsError = new TypeError("Failed to fetch");

	renderApp();

	expect(await screen.findByText("Could not load projects.", undefined, { timeout: 3_000 })).toBeInTheDocument();
	expect(await screen.findByText("Failed to fetch")).toBeInTheDocument();
	expect(screen.queryByText("No projects yet.")).not.toBeInTheDocument();
});

test("renders projects and sessions from the API", async () => {
	mockData.projects = [{ id: "proj-1", name: "my-app", path: "/home/me/my-app", sessionPrefix: "" }];
	mockData.sessions = [
		{
			id: "sess-1",
			projectId: "proj-1",
			displayName: "fix-bug",
			harness: "claude-code",
			status: "working",
			isTerminated: false,
			updatedAt: new Date().toISOString(),
		},
	];

	renderApp();

	expect(await screen.findByRole("button", { name: "Select my-app" })).toBeInTheDocument();
	expect(await screen.findByRole("button", { name: "fix-bug" })).toBeInTheDocument();
});

test("adds a project from the rail", async () => {
	const user = userEvent.setup();
	const bridge = window.ao;
	if (!bridge) throw new Error("test preload bridge is not installed");
	bridge.app.chooseDirectory = vi.fn(async () => "/Users/me/new-project");
	postMock.mockResolvedValueOnce({
		data: {
			project: {
				id: "new-project",
				name: "New Project",
				path: "/Users/me/new-project",
				repo: "git@example.com:new-project.git",
				defaultBranch: "main",
			},
		},
	});

	renderApp();

	await user.click(await screen.findByRole("button", { name: "New project" }));

	expect(bridge.app.chooseDirectory).toHaveBeenCalled();
	expect(postMock).toHaveBeenCalledWith("/api/v1/projects", { body: { path: "/Users/me/new-project" } });
	// Scope to the sidebar row: the orchestrator empty-state copy also names the project.
	expect(await screen.findByRole("button", { name: "Select New Project" })).toBeInTheDocument();
});

test("spawns a worker from the New worker modal", async () => {
	const user = userEvent.setup();
	mockData.projects = [{ id: "proj-1", name: "my-app", path: "/home/me/my-app", sessionPrefix: "" }];
	postMock.mockResolvedValueOnce({
		data: {
			session: {
				id: "new-task",
				projectId: "proj-1",
				harness: "claude-code",
				branch: "main",
				isTerminated: false,
			},
		},
	});

	renderApp();

	// Wait for projects to load.
	await screen.findByRole("button", { name: "Select my-app" });

	// Open spawn modal from the orchestrator topbar.
	await user.click(screen.getByRole("button", { name: "New worker" }));
	await user.type(await screen.findByLabelText("Prompt"), "Make task creation work");
	await user.click(screen.getByRole("button", { name: /Spawn worker/ }));

	// No `branch` field: it names the worktree branch, and sending the default
	// base ("main") makes the daemon 409 with BRANCH_CHECKED_OUT_ELSEWHERE.
	expect(postMock).toHaveBeenCalledWith("/api/v1/sessions", {
		body: {
			projectId: "proj-1",
			kind: "worker",
			harness: "claude-code",
			prompt: "Make task creation work",
		},
	});
	// No worker name given, so no rename round-trip.
	expect(patchMock).not.toHaveBeenCalled();
});

test("renames the spawned worker when a name is given", async () => {
	const user = userEvent.setup();
	mockData.projects = [{ id: "proj-1", name: "my-app", path: "/home/me/my-app", sessionPrefix: "" }];
	postMock.mockResolvedValueOnce({
		data: {
			session: {
				id: "new-task",
				projectId: "proj-1",
				harness: "claude-code",
				isTerminated: false,
			},
		},
	});
	patchMock.mockResolvedValueOnce({
		data: { ok: true, sessionId: "new-task", displayName: "fix-login" },
	});

	renderApp();

	await screen.findByRole("button", { name: "Select my-app" });

	await user.click(screen.getByRole("button", { name: "New worker" }));
	await user.type(await screen.findByLabelText("Worker name"), "fix-login");
	await user.type(screen.getByLabelText("Prompt"), "Fix the login bug");
	await user.click(screen.getByRole("button", { name: /Spawn worker/ }));

	expect(patchMock).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}", {
		params: { path: { sessionId: "new-task" } },
		body: { displayName: "fix-login" },
	});
	expect(await screen.findByRole("button", { name: "fix-login" })).toBeInTheDocument();
});

test("archives a terminated worker from its sidebar row menu", async () => {
	const user = userEvent.setup();
	mockData.projects = [{ id: "proj-1", name: "my-app", path: "/home/me/my-app", sessionPrefix: "" }];
	mockData.sessions = [
		{
			id: "sess-1",
			projectId: "proj-1",
			displayName: "old-task",
			status: "terminated",
			isTerminated: true,
			updatedAt: new Date().toISOString(),
		},
	];
	postMock.mockImplementationOnce(async () => {
		// The daemon stamps archived_at; the post-action refetch sees it.
		mockData.sessions[0].isArchived = true;
		return { data: { ok: true, sessionId: "sess-1" } };
	});

	renderApp();

	fireEvent.contextMenu(await screen.findByRole("button", { name: "old-task" }));
	await user.click(await screen.findByRole("menuitem", { name: "Archive worker" }));

	expect(postMock).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/archive", {
		params: { path: { sessionId: "sess-1" } },
	});
	// The row leaves the default worker list and lands behind the collapsed
	// Archived disclosure.
	expect(await screen.findByRole("button", { name: "Archived workers in my-app" })).toBeInTheDocument();
	expect(screen.queryByRole("button", { name: "old-task" })).not.toBeInTheDocument();
});

test("unarchives a worker from the Archived group", async () => {
	const user = userEvent.setup();
	mockData.projects = [{ id: "proj-1", name: "my-app", path: "/home/me/my-app", sessionPrefix: "" }];
	mockData.sessions = [
		{
			id: "sess-1",
			projectId: "proj-1",
			displayName: "old-task",
			status: "terminated",
			isTerminated: true,
			isArchived: true,
			updatedAt: new Date().toISOString(),
		},
	];
	postMock.mockImplementationOnce(async () => {
		mockData.sessions[0].isArchived = false;
		return { data: { ok: true, sessionId: "sess-1" } };
	});

	renderApp();

	// Archived rows stay hidden until the disclosure is expanded.
	const disclosure = await screen.findByRole("button", { name: "Archived workers in my-app" });
	expect(screen.queryByRole("button", { name: "old-task" })).not.toBeInTheDocument();
	await user.click(disclosure);

	fireEvent.contextMenu(await screen.findByRole("button", { name: "old-task" }));
	await user.click(await screen.findByRole("menuitem", { name: "Unarchive worker" }));

	expect(postMock).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/unarchive", {
		params: { path: { sessionId: "sess-1" } },
	});
	// Back in the default list; the empty disclosure disappears.
	expect(await screen.findByRole("button", { name: "old-task" })).toBeInTheDocument();
	expect(screen.queryByRole("button", { name: "Archived workers in my-app" })).not.toBeInTheDocument();
});

test("surfaces an error when spawning fails", async () => {
	const user = userEvent.setup();
	mockData.projects = [{ id: "proj-1", name: "my-app", path: "/home/me/my-app", sessionPrefix: "" }];
	postMock.mockResolvedValueOnce({ error: new TypeError("Failed to fetch") });

	renderApp();

	await screen.findByRole("button", { name: "Select my-app" });

	await user.click(screen.getByRole("button", { name: "New worker" }));
	await user.type(await screen.findByLabelText("Prompt"), "Failing task");
	await user.click(screen.getByRole("button", { name: /Spawn worker/ }));

	expect(postMock).toHaveBeenCalledWith("/api/v1/sessions", {
		body: {
			projectId: "proj-1",
			kind: "worker",
			harness: "claude-code",
			prompt: "Failing task",
		},
	});
	expect(await screen.findByText("Failed to fetch")).toBeInTheDocument();
});

test("surfaces the daemon error envelope message, not [object Object]", async () => {
	const user = userEvent.setup();
	mockData.projects = [{ id: "proj-1", name: "my-app", path: "/home/me/my-app", sessionPrefix: "" }];
	// openapi-fetch resolves non-2xx bodies as a plain APIError envelope.
	postMock.mockResolvedValueOnce({
		error: {
			code: "BRANCH_CHECKED_OUT_ELSEWHERE",
			error: "Conflict",
			message: "main is checked out at /home/me/my-app",
		},
	});

	renderApp();

	await screen.findByRole("button", { name: "Select my-app" });

	await user.click(screen.getByRole("button", { name: "New worker" }));
	await user.type(await screen.findByLabelText("Prompt"), "Failing task");
	await user.click(screen.getByRole("button", { name: /Spawn worker/ }));

	expect(
		await screen.findByText("main is checked out at /home/me/my-app (BRANCH_CHECKED_OUT_ELSEWHERE)"),
	).toBeInTheDocument();
	expect(screen.queryByText("[object Object]")).not.toBeInTheDocument();
});
