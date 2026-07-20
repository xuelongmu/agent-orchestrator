import * as Dialog from "@radix-ui/react-dialog";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Loader2, X } from "lucide-react";
import { type FormEvent, useEffect, useId, useState } from "react";
import { Button } from "./ui/button";
import { Input } from "./ui/input";
import { Label } from "./ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "./ui/select";
import { RequiredAgentField } from "./CreateProjectAgentSheet";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage } from "../lib/api-client";
import { captureRendererEvent } from "../lib/telemetry";
import type { AgentProvider, WorkspaceKind } from "../types/workspace";
import { agentsQueryKey, agentsQueryOptions, refreshAgents, type AgentCatalog } from "../hooks/useAgentsQuery";

type Project = components["schemas"]["Project"];

type NewTaskDialogProps = {
	open: boolean;
	projectId?: string;
	onCreated: (sessionId: string) => void;
	onOpenChange: (open: boolean) => void;
};

export function NewTaskDialog({ open, projectId, onCreated, onOpenChange }: NewTaskDialogProps) {
	const queryClient = useQueryClient();
	const titleId = useId();
	const promptId = useId();
	const branchId = useId();
	const workspaceKindId = useId();
	const agentId = useId();
	const [title, setTitle] = useState("");
	const [prompt, setPrompt] = useState("");
	const [branch, setBranch] = useState("");
	const [workspaceKind, setWorkspaceKind] = useState<WorkspaceKind | "">("");
	const [agent, setAgent] = useState("");
	const [agentTouched, setAgentTouched] = useState(false);
	const [isSubmitting, setIsSubmitting] = useState(false);
	const [error, setError] = useState<string | undefined>();

	const projectQuery = useQuery({
		queryKey: ["project", projectId],
		enabled: open && Boolean(projectId),
		queryFn: async () => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/projects/{id}", {
				params: { path: { id: projectId as string } },
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
			if (data?.status !== "ok") throw new Error("Project config is unavailable.");
			return data.project as Project;
		},
	});
	const agentsQuery = useQuery({
		...agentsQueryOptions,
		enabled: open,
	});
	const refreshAgentsMutation = useMutation({
		mutationFn: refreshAgents,
		onSuccess: (next) => queryClient.setQueryData(agentsQueryKey, next),
	});
	const defaultWorkerAgent = projectQuery.data?.config?.worker?.agent ?? "";
	const effectiveWorkspaceKind = workspaceKind || projectQuery.data?.config?.workspaceKind || "worktree";
	const agentCatalog = agentsQuery.data;
	const effectiveAgent = agent || defaultWorkerAgent || projectQuery.data?.agent || "";
	const configuredOrchestratorAgent = projectQuery.data?.config?.orchestrator?.agent || projectQuery.data?.agent || "";

	useEffect(() => {
		if (!open) {
			setTitle("");
			setPrompt("");
			setBranch("");
			setWorkspaceKind("");
			setAgent("");
			setAgentTouched(false);
			setError(undefined);
			setIsSubmitting(false);
		}
	}, [open]);

	useEffect(() => {
		if (open && !agentTouched) {
			setAgent(defaultWorkerAgent);
		}
	}, [open, agentTouched, defaultWorkerAgent]);

	const submit = async (event: FormEvent<HTMLFormElement>) => {
		event.preventDefault();
		if (!projectId || !projectQuery.data || isSubmitting) return;

		const cleanTitle = title.trim();
		const cleanPrompt = prompt.trim();
		const cleanBranch = branch.trim();
		if (!cleanTitle || !cleanPrompt) {
			setError("Title and brief are required.");
			return;
		}

		setIsSubmitting(true);
		setError(undefined);
		void captureRendererEvent("ao.renderer.task_create_requested", { project_id: projectId });
		try {
			const { data, error: apiError } = await apiClient.POST("/api/v1/sessions", {
				body: {
					projectId,
					kind: "worker",
					harness: agentTouched && agent ? (agent as AgentProvider) : undefined,
					issueId: cleanTitle,
					prompt: cleanPrompt,
					// Omit the kind until the user explicitly overrides it. The daemon
					// then resolves the durable project default even if this dialog was
					// submitted before the project query finished loading.
					workspaceKind: workspaceKind || undefined,
					branch: effectiveWorkspaceKind === "worktree" ? cleanBranch || undefined : undefined,
				},
			});
			if (apiError) throw new Error(apiErrorMessage(apiError, "Unable to start task"));
			if (!data?.session?.id) throw new Error("Task creation returned no session");
			void captureRendererEvent("ao.renderer.task_create_succeeded", { project_id: projectId });
			onCreated(data.session.id);
			onOpenChange(false);
		} catch (err) {
			void captureRendererEvent("ao.renderer.task_create_failed", { project_id: projectId });
			void queryClient.invalidateQueries({ queryKey: agentsQueryKey });
			setError(err instanceof Error ? err.message : "Unable to start task");
		} finally {
			setIsSubmitting(false);
		}
	};

	return (
		<Dialog.Root open={open} onOpenChange={onOpenChange}>
			<Dialog.Portal>
				<Dialog.Overlay className="fixed inset-0 z-overlay bg-scrim data-[state=open]:animate-overlay-in" />
				<Dialog.Content className="fixed left-1/2 top-1/2 z-overlay w-dialog-xl -translate-x-1/2 -translate-y-1/2 rounded-lg border border-border bg-popover p-0 text-popover-foreground shadow-xl data-[state=open]:animate-modal-in">
					<div className="flex items-start justify-between gap-4 border-b border-border px-5 py-4">
						<div className="min-w-0">
							<Dialog.Title className="text-subtitle font-semibold text-foreground">New task</Dialog.Title>
							<Dialog.Description className="mt-1 text-xs text-muted-foreground">
								Start a worker directly from this project.
							</Dialog.Description>
						</div>
						<Dialog.Close asChild>
							<button
								type="button"
								className="grid size-7 shrink-0 place-items-center rounded-md text-muted-foreground transition hover:bg-surface hover:text-foreground"
								aria-label="Close new task dialog"
							>
								<X className="size-icon-base" aria-hidden="true" />
							</button>
						</Dialog.Close>
					</div>

					<form onSubmit={submit} className="space-y-4 px-5 py-4">
						<div
							aria-busy={projectQuery.isPending}
							aria-label="Task execution context"
							className="rounded-md border border-border bg-surface px-3 py-2.5 text-xs"
							data-testid="task-execution-context"
							role="group"
						>
							{projectQuery.data ? (
								<TaskExecutionContext
									agentCatalog={agentCatalog}
									orchestratorAgent={configuredOrchestratorAgent}
									project={projectQuery.data}
									workerAgent={effectiveAgent}
								/>
							) : projectQuery.isPending ? (
								<p className="text-muted-foreground" role="status">
									Loading project context…
								</p>
							) : (
								<div role="alert">
									<p className="font-medium text-destructive">Project context unavailable.</p>
									<p className="mt-0.5 text-caption text-muted-foreground">
										{projectQuery.error instanceof Error ? projectQuery.error.message : "Project lookup failed."}
									</p>
								</div>
							)}
						</div>

						<div className="space-y-1.5">
							<label className="text-xs font-medium text-muted-foreground" htmlFor={titleId}>
								Title
							</label>
							<Input
								id={titleId}
								autoFocus
								placeholder="Fix WebGL fallback renderer"
								value={title}
								onChange={(event) => setTitle(event.target.value)}
							/>
						</div>

						<div className="space-y-1.5">
							<label className="text-xs font-medium text-muted-foreground" htmlFor={promptId}>
								Brief
							</label>
							<textarea
								id={promptId}
								className="min-h-textarea-min w-full resize-y rounded-md border border-border bg-transparent px-3 py-2 text-control leading-relaxed text-foreground outline-none transition placeholder:text-passive focus-visible:border-accent focus-visible:ring-2 focus-visible:ring-accent-weak"
								placeholder="Describe the change, constraints, and expected verification."
								value={prompt}
								onChange={(event) => setPrompt(event.target.value)}
							/>
						</div>

						<div className="grid gap-3 sm:grid-cols-3">
							<div className="space-y-1.5">
								<RequiredAgentField
									id={agentId}
									label="Agent"
									placeholder="Project default"
									value={agent}
									authorized={agentCatalog?.authorized}
									installed={agentCatalog?.installed}
									supported={agentCatalog?.supported}
									disabled={agentsQuery.isFetching && agentCatalog === undefined}
									onChange={(value) => {
										setAgent(value);
										setAgentTouched(true);
									}}
								/>
								<button
									type="button"
									className="text-xs text-muted-foreground underline-offset-2 hover:text-foreground hover:underline disabled:pointer-events-none disabled:opacity-50"
									disabled={refreshAgentsMutation.isPending}
									onClick={() => refreshAgentsMutation.mutate()}
								>
									{refreshAgentsMutation.isPending ? "Refreshing agents..." : "Refresh agents"}
								</button>
							</div>
							<div className="space-y-1.5">
								<Label className="text-xs font-medium text-muted-foreground" htmlFor={workspaceKindId}>
									Workspace
								</Label>
								<Select
									value={effectiveWorkspaceKind}
									onValueChange={(value) => setWorkspaceKind(value as WorkspaceKind)}
								>
									<SelectTrigger id={workspaceKindId} size="sm" aria-label="Workspace">
										<SelectValue />
									</SelectTrigger>
									<SelectContent>
										<SelectItem value="worktree">Git worktree</SelectItem>
										<SelectItem value="scratch">Scratch</SelectItem>
										<SelectItem value="dir">Project directory</SelectItem>
									</SelectContent>
								</Select>
							</div>
							<div className="space-y-1.5">
								<Label className="text-xs font-medium text-muted-foreground" htmlFor={branchId}>
									Branch
								</Label>
								<Input
									id={branchId}
									placeholder="optional"
									value={branch}
									disabled={effectiveWorkspaceKind !== "worktree"}
									onChange={(event) => setBranch(event.target.value)}
								/>
							</div>
						</div>

						{error && (
							<div className="rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-xs text-destructive">
								{error}
							</div>
						)}

						{refreshAgentsMutation.isError && (
							<div className="rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-xs text-destructive">
								{refreshAgentsMutation.error instanceof Error
									? refreshAgentsMutation.error.message
									: "Could not refresh agent catalog."}
							</div>
						)}

						<div className="flex items-center justify-end gap-2 pt-1">
							<Dialog.Close asChild>
								<Button type="button" variant="ghost" disabled={isSubmitting}>
									Cancel
								</Button>
							</Dialog.Close>
							<Button type="submit" disabled={isSubmitting || !projectId || !projectQuery.data}>
								{isSubmitting ? <Loader2 className="size-3.5 animate-spin" aria-hidden="true" /> : null}
								{isSubmitting ? "Starting..." : "Start task"}
							</Button>
						</div>
					</form>
				</Dialog.Content>
			</Dialog.Portal>
		</Dialog.Root>
	);
}

function TaskExecutionContext({
	agentCatalog,
	orchestratorAgent,
	project,
	workerAgent,
}: {
	agentCatalog?: AgentCatalog;
	orchestratorAgent: string;
	project: Project;
	workerAgent: string;
}) {
	const repositories = projectRepositories(project);
	return (
		<>
			<div className="font-semibold text-foreground">{project.name}</div>
			<RepositoryList repositories={repositories} />
			<dl className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-caption">
				<ContextFact label="Base branch" value={project.defaultBranch} mono />
				{workerAgent ? <ContextFact label="Worker" value={agentDisplayName(workerAgent, agentCatalog)} /> : null}
				{orchestratorAgent ? (
					<ContextFact label="Orchestrator" value={agentDisplayName(orchestratorAgent, agentCatalog)} />
				) : null}
			</dl>
			<ProjectPath path={project.path} />
		</>
	);
}

function RepositoryList({ repositories }: { repositories: string[] }) {
	if (repositories.length === 0) return null;
	return (
		<div className="mt-1 flex min-w-0 items-start gap-1.5 text-caption">
			<span className="shrink-0 text-muted-foreground">
				{repositories.length === 1 ? "Repository" : "Repositories"}
			</span>
			<ul aria-label="Repositories" className="min-w-0 space-y-0.5 text-foreground">
				{repositories.map((repository) => (
					<li className="break-all" key={repository}>
						{repository}
					</li>
				))}
			</ul>
		</div>
	);
}

function ProjectPath({ path }: { path: string }) {
	return (
		<div className="mt-2 flex min-w-0 items-start gap-1.5 border-t border-border pt-2 text-caption">
			<span className="shrink-0 text-muted-foreground">Path</span>
			<code className="break-all font-mono text-2xs text-muted-foreground">{path}</code>
		</div>
	);
}

function projectRepositories(project: Project): string[] {
	return [
		...new Set([project.repo, ...(project.workspaceRepos ?? []).map((repository) => repository.repo)].filter(Boolean)),
	];
}

function ContextFact({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
	return (
		<div className="flex min-w-0 items-baseline gap-1.5">
			<dt className="shrink-0 text-muted-foreground">{label}</dt>
			<dd className={mono ? "truncate font-mono text-foreground" : "truncate font-medium text-foreground"}>{value}</dd>
		</div>
	);
}

function agentDisplayName(agentId: string, catalog?: AgentCatalog): string {
	const knownAgent = [...(catalog?.supported ?? []), ...(catalog?.installed ?? [])].find(
		(agent) => agent.id === agentId,
	);
	if (knownAgent) return knownAgent.label;
	return agentId
		.split("-")
		.map((part) => part.charAt(0).toUpperCase() + part.slice(1))
		.join(" ");
}
