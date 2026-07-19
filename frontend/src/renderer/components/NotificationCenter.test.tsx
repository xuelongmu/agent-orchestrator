import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { NotificationCenter } from "./NotificationCenter";
import type { NotificationDTO } from "../lib/notifications";

const notifications: NotificationDTO[] = [
	{
		id: "ntf_1",
		sessionId: "sess-1",
		projectId: "proj-1",
		prUrl: "",
		type: "needs_input",
		title: "Needs input",
		body: "",
		status: "unread",
		createdAt: "2026-06-16T10:00:00Z",
		target: { kind: "session", sessionId: "sess-1" },
	},
	{
		id: "ntf_2",
		sessionId: "sess-2",
		projectId: "proj-1",
		prUrl: "",
		type: "ready_to_merge",
		title: "Ready to merge",
		body: "",
		status: "unread",
		createdAt: "2026-06-16T11:00:00Z",
		target: { kind: "session", sessionId: "sess-2" },
	},
	{
		id: "ntf_3",
		sessionId: "",
		projectId: "",
		prUrl: "",
		type: "control_plane_failed",
		title: "GitHub authentication needs attention",
		body: "Update GitHub authentication.",
		status: "unread",
		createdAt: "2026-06-16T12:00:00Z",
		target: { kind: "control_plane", sessionId: "" },
	},
];

vi.mock("@tanstack/react-router", () => ({ useNavigate: () => vi.fn() }));

vi.mock("../hooks/useNotificationsQuery", () => ({
	useMarkAllNotificationsReadMutation: () => ({ isPending: false, mutateAsync: vi.fn() }),
	useMarkNotificationReadMutation: () => ({ isPending: false, mutateAsync: vi.fn() }),
	useNotificationsQuery: () => ({ data: notifications, isError: false }),
}));

vi.mock("../lib/notifications", async (importOriginal) => ({
	...((await importOriginal()) as object),
	createNotificationsTransport: () => ({ connect: () => undefined }),
}));

function renderNotificationCenter() {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(
		<QueryClientProvider client={queryClient}>
			<NotificationCenter />
		</QueryClientProvider>,
	);
}

describe("NotificationCenter", () => {
	it("renders a filled bell with a text-only yellow unread count", () => {
		renderNotificationCenter();

		const trigger = screen.getByRole("button", { name: "3 unread notifications" });
		const bell = trigger.querySelector("svg");
		const count = screen.getByText("3");

		expect(bell).toHaveClass("fill-current");
		expect(count).toHaveClass("text-caption");
		expect(count).toHaveClass("text-warning");
		expect(count).not.toHaveClass("bg-warning");
		expect(count).not.toHaveClass("rounded-full");
		expect(count).not.toHaveClass("text-background");
	});

	it("does not offer an open action for control-plane notifications", () => {
		renderNotificationCenter();
		fireEvent.click(screen.getByRole("button", { name: "3 unread notifications" }));

		const title = screen.getByText("GitHub authentication needs attention");
		const item = title.closest(".grid");
		expect(item).not.toBeNull();
		expect(within(item as HTMLElement).queryByTitle("Open target")).not.toBeInTheDocument();
		expect(screen.getAllByTitle("Open target")).toHaveLength(2);
	});
});
