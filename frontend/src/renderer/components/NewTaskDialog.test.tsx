import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NewTaskDialog } from "./NewTaskDialog";

const { getMock, postMock } = vi.hoisted(() => ({
	getMock: vi.fn(),
	postMock: vi.fn(),
}));

vi.mock("../lib/api-client", () => ({
	apiClient: {
		GET: (...args: unknown[]) => getMock(...args),
		POST: (...args: unknown[]) => postMock(...args),
	},
	apiErrorMessage: (error: unknown, fallback = "Request failed") => {
		if (typeof error === "object" && error !== null && "message" in error) {
			const body = error as { code?: unknown; message: unknown };
			const message = String(body.message);
			return typeof body.code === "string" && body.code !== "" ? `${message} (${body.code})` : message;
		}
		return fallback;
	},
}));

function renderDialog() {
	const onCreated = vi.fn();
	const onOpenChange = vi.fn();
	render(
		<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
			<NewTaskDialog open projectId="proj-1" onCreated={onCreated} onOpenChange={onOpenChange} />
		</QueryClientProvider>,
	);
	return { onCreated, onOpenChange };
}

function spawnBody() {
	return (postMock.mock.calls[0][1] as { body: Record<string, unknown> }).body;
}

async function waitForAgentCatalog() {
	await waitFor(() => expect(screen.getAllByText("Claude Code").length).toBeGreaterThan(0));
}

beforeEach(() => {
	getMock.mockReset().mockImplementation(async (path: string) => {
		if (path === "/api/v1/agents") {
			return {
				data: {
					supported: [
						{ id: "claude-code", label: "Claude Code" },
						{ id: "cursor", label: "Cursor" },
						{ id: "kiro", label: "Kiro" },
					],
					installed: [
						{ id: "claude-code", label: "Claude Code", authStatus: "authorized" },
						{ id: "cursor", label: "Cursor", authStatus: "authorized" },
						{ id: "kiro", label: "Kiro", authStatus: "unknown" },
					],
					authorized: [
						{ id: "claude-code", label: "Claude Code", authStatus: "authorized" },
						{ id: "cursor", label: "Cursor", authStatus: "authorized" },
					],
				},
				error: undefined,
			};
		}
		return {
			data: {
				status: "ok",
				project: {
					id: "proj-1",
					name: "CareerOps",
					repo: "github.com/acme/careerops",
					path: "/repos/careerops",
					defaultBranch: "main",
					config: {
						worker: { agent: "claude-code" },
						orchestrator: { agent: "codex" },
					},
				},
			},
			error: undefined,
		};
	});
	postMock.mockReset().mockResolvedValue({ data: { session: { id: "task-1" } }, error: undefined });
});

afterEach(() => vi.restoreAllMocks());

describe("NewTaskDialog", () => {
	it("shows the execution context before the task is launched", async () => {
		renderDialog();

		await screen.findByText("CareerOps");
		const context = within(screen.getByRole("group", { name: "Task execution context" }));
		expect(context.getByText("CareerOps")).toBeInTheDocument();
		expect(context.getByText("github.com/acme/careerops")).toBeInTheDocument();
		expect(context.getByText("main")).toBeInTheDocument();
		expect(context.getByText("Claude Code")).toBeInTheDocument();
		expect(context.getByText("Codex")).toBeInTheDocument();
		expect(context.getByText("Path")).toBeInTheDocument();
		expect(context.getByText("/repos/careerops", { selector: "code" })).toBeVisible();
	});

	it("keeps a many-repository workspace within a scrollable viewport-bounded dialog", async () => {
		const originalImplementation = getMock.getMockImplementation();
		const workspaceRepos = Array.from({ length: 12 }, (_, index) => ({
			name: `service-${index + 1}`,
			relativePath: `services/service-${index + 1}`,
			repo: `github.com/acme/service-${index + 1}`,
		}));
		getMock.mockImplementation((path: string) => {
			if (path === "/api/v1/projects/{id}") {
				return Promise.resolve({
					data: {
						status: "ok",
						project: {
							id: "proj-1",
							name: "Product suite",
							repo: "",
							path: "/repos/product-suite",
							defaultBranch: "main",
							agent: "claude-code",
							workspaceRepos,
						},
					},
					error: undefined,
				});
			}
			return originalImplementation?.(path);
		});

		renderDialog();

		const repositories = within(await screen.findByRole("list", { name: "Repositories" }));
		expect(repositories.getAllByRole("listitem")).toHaveLength(12);
		const dialog = screen.getByRole("dialog", { name: "New task" });
		expect(dialog).toHaveClass("flex", "max-h-[min(720px,calc(100svh-24px))]", "overflow-hidden");
		expect(dialog.querySelector("form")).toHaveClass("min-h-0", "flex-1", "overflow-y-auto");
	});

	it("aligns the Agent, Workspace, and Branch fields with matching labels and compact controls", async () => {
		renderDialog();
		await waitForAgentCatalog();

		const agentLabel = screen.getByText("Agent", { selector: "label" });
		const branchLabel = screen.getByText("Branch", { selector: "label" });
		const workspaceLabel = screen.getByText("Workspace", { selector: "label" });
		expect(agentLabel).toHaveAttribute("data-slot", "label");
		expect(branchLabel).toHaveAttribute("data-slot", "label");
		expect(workspaceLabel).toHaveAttribute("data-slot", "label");
		expect(screen.getByRole("combobox", { name: "Agent" })).toHaveAttribute("data-size", "sm");
		expect(screen.getByLabelText("Branch")).toHaveClass("h-control-form");
	});

	it("preselects the project's default agent and omits harness so the daemon applies it", async () => {
		const { onCreated, onOpenChange } = renderDialog();
		const user = userEvent.setup();

		await waitForAgentCatalog();

		await user.type(screen.getByLabelText("Title"), "Fix fallback renderer");
		await user.type(screen.getByLabelText("Brief"), "Restore the fallback renderer after WebGL init fails.");
		await user.click(screen.getByRole("button", { name: "Start task" }));

		await waitFor(() => expect(postMock).toHaveBeenCalledTimes(1));
		expect(postMock).toHaveBeenCalledWith("/api/v1/sessions", {
			body: {
				projectId: "proj-1",
				kind: "worker",
				harness: undefined,
				issueId: "Fix fallback renderer",
				prompt: "Restore the fallback renderer after WebGL init fails.",
				workspaceKind: undefined,
				branch: undefined,
			},
		});
		expect(onCreated).toHaveBeenCalledWith("task-1");
		expect(onOpenChange).toHaveBeenCalledWith(false);
	}, 20_000);

	it("shows pending project context and prevents launch while it is loading", async () => {
		const originalImplementation = getMock.getMockImplementation();
		getMock.mockImplementation((path: string) => {
			if (path === "/api/v1/projects/{id}") {
				return new Promise(() => {});
			}
			return originalImplementation?.(path);
		});

		renderDialog();

		expect(await screen.findByText("Loading project context…")).toBeInTheDocument();
		expect(screen.getByRole("button", { name: "Start task" })).toBeDisabled();
		expect(postMock).not.toHaveBeenCalled();
	});

	it("shows a project context error and prevents launch when project detail is unavailable", async () => {
		const originalImplementation = getMock.getMockImplementation();
		getMock.mockImplementation((path: string) => {
			if (path === "/api/v1/projects/{id}") {
				return Promise.resolve({ data: undefined, error: { message: "project lookup failed" } });
			}
			return originalImplementation?.(path);
		});

		renderDialog();

		expect(await screen.findByText("Project context unavailable.")).toBeInTheDocument();
		expect(screen.getByText("project lookup failed")).toBeInTheDocument();
		expect(screen.getByRole("button", { name: "Start task" })).toBeDisabled();
		expect(postMock).not.toHaveBeenCalled();
	});

	it("spawns a branchless scratch task", async () => {
		renderDialog();
		const user = userEvent.setup();
		await waitForAgentCatalog();

		await user.click(screen.getByRole("combobox", { name: "Workspace" }));
		await user.click(await screen.findByRole("option", { name: "Scratch" }));
		expect(screen.getByLabelText("Branch")).toBeDisabled();
		await user.type(screen.getByLabelText("Title"), "Research");
		await user.type(screen.getByLabelText("Brief"), "Compare the available approaches.");
		await user.click(screen.getByRole("button", { name: "Start task" }));

		await waitFor(() => expect(postMock).toHaveBeenCalledTimes(1));
		expect(spawnBody().workspaceKind).toBe("scratch");
		expect(spawnBody().branch).toBeUndefined();
	});

	it("sends the chosen harness when the user overrides the default", async () => {
		renderDialog();
		const user = userEvent.setup();
		await waitForAgentCatalog();

		await user.type(screen.getByLabelText("Title"), "T");
		await user.type(screen.getByLabelText("Brief"), "B");

		await user.click(screen.getByRole("combobox", { name: "Agent" }));
		await user.click(await screen.findByRole("option", { name: "Cursor" }));

		await user.click(screen.getByRole("button", { name: "Start task" }));

		await waitFor(() => expect(postMock).toHaveBeenCalledTimes(1));
		expect(spawnBody().harness).toBe("cursor");
	});

	it("allows selecting an installed agent with unknown auth", async () => {
		renderDialog();
		const user = userEvent.setup();
		await waitForAgentCatalog();

		await user.click(screen.getByRole("combobox", { name: "Agent" }));
		const options = await screen.findAllByRole("option");
		expect(options.map((option) => option.textContent)).toEqual(["Claude Code", "Cursor", "KiroAuth unknown"]);
		expect(options[2]).not.toHaveAttribute("aria-disabled", "true");
		await user.click(options[2]);

		await user.type(screen.getByLabelText("Title"), "T");
		await user.type(screen.getByLabelText("Brief"), "B");
		await user.click(screen.getByRole("button", { name: "Start task" }));

		await waitFor(() => expect(postMock).toHaveBeenCalledTimes(1));
		expect(spawnBody().harness).toBe("kiro");
	});

	it("requires both title and brief", async () => {
		renderDialog();
		const user = userEvent.setup();

		await user.click(screen.getByRole("button", { name: "Start task" }));

		expect(await screen.findByText("Title and brief are required.")).toBeInTheDocument();
		expect(postMock).not.toHaveBeenCalled();
	});

	it.each([
		{
			code: "AGENT_BINARY_NOT_FOUND",
			message: "agent binary not found on PATH",
		},
		{
			code: "RUNTIME_PREREQUISITE_MISSING",
			message: "tmux required on macOS/Linux but not in PATH",
		},
		{
			code: "INTERNAL",
			message: "runtime launch failed",
		},
	])("displays daemon spawn errors for $code", async ({ code, message }) => {
		postMock.mockResolvedValueOnce({
			data: undefined,
			error: { code, message },
		});
		renderDialog();
		const user = userEvent.setup();
		await waitForAgentCatalog();

		await user.type(screen.getByLabelText("Title"), "Fix fallback renderer");
		await user.type(screen.getByLabelText("Brief"), "Restore fallback renderer.");
		await user.click(screen.getByRole("button", { name: "Start task" }));

		expect(await screen.findByText(`${message} (${code})`)).toBeInTheDocument();
	});
});
