import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { CenterPane } from "./components/CenterPane";
import { SideRail } from "./components/SideRail";
import { Sidebar } from "./components/Sidebar";
import { SpawnWorkerModal } from "./components/SpawnWorkerModal";
import { Topbar } from "./components/Topbar";
import { useDaemonStatus } from "./hooks/useDaemonStatus";
import { useWorkspaceQuery, workspaceQueryKey } from "./hooks/useWorkspaceQuery";
import { apiClient } from "./lib/api-client";
import { apiErrorMessage } from "./lib/api-errors";
import { Theme, useUiStore } from "./stores/ui-store";
import { toAgentProvider, toSessionStatus, type AgentProvider, type WorkspaceSummary } from "./types/workspace";

type AppProps = {
	routeSessionId?: string;
	routeWorkspaceId?: string;
};

function systemTheme(): Theme {
	return window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
}

function errorMessage(error: unknown) {
	return error instanceof Error ? error.message : "Could not load projects";
}

export function App({ routeSessionId, routeWorkspaceId }: AppProps) {
	const queryClient = useQueryClient();
	const {
		view,
		workbenchTab,
		selectedSessionId,
		selectedWorkspaceId,
		theme,
		setSystemTheme,
		setWorkbenchTab,
		toggleSidebar,
		selectWorkspace,
		selectSession,
	} = useUiStore();
	const workspaceQuery = useWorkspaceQuery();
	const workspaces = workspaceQuery.data ?? [];
	const daemonStatus = useDaemonStatus(queryClient);
	const [spawnOpen, setSpawnOpen] = useState(false);
	const [spawnProjectId, setSpawnProjectId] = useState<string | undefined>(undefined);

	const openSpawn = (projectId?: string) => {
		setSpawnProjectId(projectId);
		setSpawnOpen(true);
	};

	const selectedWorkspace =
		workspaces.find((workspace) => workspace.id === selectedWorkspaceId) ?? workspaces[0] ?? null;
	const selectedSession =
		view === "session"
			? workspaces
					.flatMap((workspace) => [...workspace.sessions, ...(workspace.archivedSessions ?? [])])
					.find((session) => session.id === selectedSessionId)
			: undefined;
	const sessionWorkspace = selectedSession
		? (workspaces.find((workspace) => workspace.id === selectedSession.workspaceId) ?? selectedWorkspace)
		: selectedWorkspace;

	useEffect(() => {
		if (routeWorkspaceId) selectWorkspace(routeWorkspaceId);
		if (routeSessionId) selectSession(routeSessionId, routeWorkspaceId);
	}, [routeSessionId, routeWorkspaceId, selectWorkspace, selectSession]);

	useEffect(() => {
		document.documentElement.dataset.theme = theme;
		document.documentElement.style.colorScheme = theme;
	}, [theme]);

	useEffect(() => {
		const mediaQuery = window.matchMedia("(prefers-color-scheme: light)");
		const handleChange = () => setSystemTheme(systemTheme());

		handleChange();
		mediaQuery.addEventListener("change", handleChange);
		return () => mediaQuery.removeEventListener("change", handleChange);
	}, [setSystemTheme]);

	useEffect(() => {
		const handleKeyDown = (event: KeyboardEvent) => {
			if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "b") {
				event.preventDefault();
				toggleSidebar();
				return;
			}

			if ((event.metaKey || event.ctrlKey) && /^[1-9]$/.test(event.key)) {
				const workspace = workspaces[Number(event.key) - 1];
				if (workspace) {
					event.preventDefault();
					selectWorkspace(workspace.id);
				}
			}
		};

		window.addEventListener("keydown", handleKeyDown);
		return () => window.removeEventListener("keydown", handleKeyDown);
	}, [selectWorkspace, toggleSidebar, workspaces]);

	const updateWorkspaces = (updater: (workspaces: WorkspaceSummary[]) => WorkspaceSummary[]) => {
		queryClient.setQueryData<WorkspaceSummary[]>(workspaceQueryKey, (current = workspaces) => updater(current));
	};

	const createProject = async (input: { path: string }) => {
		const { data, error } = await apiClient.POST("/api/v1/projects", { body: { path: input.path } });

		if (error) throw new Error(apiErrorMessage(error, "Could not add project"));
		if (!data?.project) throw new Error("Project creation returned no project");

		const workspace: WorkspaceSummary = {
			id: data.project.id,
			name: data.project.name,
			path: data.project.path,
			type: "main",
			sessions: [],
		};

		updateWorkspaces((current) => [workspace, ...current.filter((item) => item.id !== workspace.id)]);
		selectWorkspace(workspace.id);
	};

	const createTask = async (input: { projectId: string; prompt: string; name?: string; harness?: AgentProvider }) => {
		// No `branch` here: the API's branch field names the session's worktree
		// branch (sending a checked-out branch like "main" 409s); the base branch
		// comes from the project config.
		const { data, error } = await apiClient.POST("/api/v1/sessions", {
			body: {
				projectId: input.projectId,
				kind: "worker",
				harness: input.harness,
				prompt: input.prompt,
			},
		});

		if (error || !data?.session) {
			throw new Error(apiErrorMessage(error, "No session returned"));
		}

		const session = data.session;

		// Best-effort: the session is already running, so a failed rename should
		// not surface as a failed spawn (retrying would create a duplicate).
		let displayName: string | undefined;
		if (input.name) {
			const renamed = await apiClient.PATCH("/api/v1/sessions/{sessionId}", {
				params: { path: { sessionId: session.id } },
				body: { displayName: input.name },
			});
			displayName = renamed.data?.displayName;
		}

		updateWorkspaces((current) =>
			current.map((item) =>
				item.id === input.projectId
					? {
							...item,
							sessions: [
								{
									id: session.id,
									terminalHandleId: session.terminalHandleId,
									workspaceId: item.id,
									workspaceName: item.name,
									title: displayName ?? input.prompt,
									provider: toAgentProvider(session.harness),
									branch: "",
									status: toSessionStatus(session.status, session.isTerminated),
									updatedAt: "now",
								},
								...item.sessions.filter((existing) => existing.id !== session.id),
							],
						}
					: item,
			),
		);
		selectSession(session.id, input.projectId);
	};

	const refetchWorkspaces = () => queryClient.invalidateQueries({ queryKey: workspaceQueryKey });

	const killSession = async (sessionId: string) => {
		const { error } = await apiClient.POST("/api/v1/sessions/{sessionId}/kill", {
			params: { path: { sessionId } },
		});
		if (error) throw new Error(apiErrorMessage(error, "Could not kill worker"));
		await refetchWorkspaces();
	};

	const restoreSession = async (sessionId: string) => {
		const { error } = await apiClient.POST("/api/v1/sessions/{sessionId}/restore", {
			params: { path: { sessionId } },
		});
		if (error) throw new Error(apiErrorMessage(error, "Could not restore worker"));
		await refetchWorkspaces();
	};

	const archiveSession = async (sessionId: string) => {
		const { error } = await apiClient.POST("/api/v1/sessions/{sessionId}/archive", {
			params: { path: { sessionId } },
		});
		if (error) throw new Error(apiErrorMessage(error, "Could not archive worker"));
		await refetchWorkspaces();
	};

	const unarchiveSession = async (sessionId: string) => {
		const { error } = await apiClient.POST("/api/v1/sessions/{sessionId}/unarchive", {
			params: { path: { sessionId } },
		});
		if (error) throw new Error(apiErrorMessage(error, "Could not unarchive worker"));
		await refetchWorkspaces();
	};

	const cleanupProject = async (projectId: string) => {
		const { error } = await apiClient.POST("/api/v1/sessions/cleanup", {
			params: { query: { project: projectId } },
		});
		if (error) throw new Error(apiErrorMessage(error, "Could not clean up sessions"));
		await refetchWorkspaces();
	};

	const removeProject = async (projectId: string) => {
		const { error } = await apiClient.DELETE("/api/v1/projects/{id}", {
			params: { path: { id: projectId } },
		});
		if (error) throw new Error(apiErrorMessage(error, "Could not remove project"));
		if (selectedWorkspaceId === projectId) selectWorkspace("");
		await refetchWorkspaces();
	};

	// The orchestrator view fronts the selected project's orchestrator session;
	// spawning one is idempotent on the daemon side (one active per project).
	const startOrchestrator = async () => {
		if (!selectedWorkspace) return;
		const { error } = await apiClient.POST("/api/v1/orchestrators", {
			body: { projectId: selectedWorkspace.id },
		});
		if (error) throw new Error(apiErrorMessage(error, "Could not start orchestrator"));
		await queryClient.invalidateQueries({ queryKey: workspaceQueryKey });
	};

	const showSideRail = !(view === "session" && workbenchTab === "terminal");

	return (
		<>
			<div className="flex h-screen flex-col bg-background text-foreground">
				<Topbar
					onNewWorker={() => openSpawn()}
					onSetWorkbenchTab={setWorkbenchTab}
					onToggleSidebar={toggleSidebar}
					session={selectedSession}
					view={view}
					workbenchTab={workbenchTab}
					workspace={sessionWorkspace}
				/>
				<div className="flex min-h-0 flex-1">
					<Sidebar
						daemonStatus={daemonStatus}
						onArchiveSession={archiveSession}
						onCleanupProject={cleanupProject}
						onCreateProject={createProject}
						onKillSession={killSession}
						onNewWorker={openSpawn}
						onRemoveProject={removeProject}
						onRestoreSession={restoreSession}
						onUnarchiveSession={unarchiveSession}
						workspaceError={workspaceQuery.isError ? errorMessage(workspaceQuery.error) : undefined}
						workspaces={workspaces}
					/>
					<CenterPane
						daemonReady={daemonStatus.state === "ready"}
						onStartOrchestrator={startOrchestrator}
						session={view === "orchestrator" ? selectedWorkspace?.orchestrator : selectedSession}
						theme={theme}
						view={view}
						workspace={selectedWorkspace}
					/>
					{showSideRail && (
						<SideRail onSelectSession={selectSession} session={selectedSession} view={view} workspaces={workspaces} />
					)}
				</div>
			</div>
			<SpawnWorkerModal
				defaultProjectId={spawnProjectId ?? selectedWorkspace?.id}
				onCreateTask={createTask}
				onOpenChange={setSpawnOpen}
				open={spawnOpen}
				workspaces={workspaces}
			/>
		</>
	);
}
