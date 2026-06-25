import { describe, expect, it } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { SessionInspector } from "@/components/SessionInspector";
import { makePR, makeSession } from "@/__tests__/helpers";

describe("SessionInspector", () => {
  it("defaults to the Summary view with PR, Activity and Overview sections", () => {
    render(
      <SessionInspector
        session={makeSession({
          id: "insp-1",
          branch: "feat/x",
          pr: makePR({ number: 55, title: "Add the thing" }),
        })}
      />,
    );

    expect(screen.getByText("Pull request")).toBeInTheDocument();
    expect(screen.getByText("Activity")).toBeInTheDocument();
    expect(screen.getByText("Overview")).toBeInTheDocument();
    expect(screen.getByText("PR #55: Add the thing")).toBeInTheDocument();
  });

  it("shows the agent cost in the Overview when present", () => {
    render(
      <SessionInspector
        session={makeSession({
          id: "insp-cost",
          cost: { inputTokens: 40_000, outputTokens: 5_200, estimatedCostUsd: 1.23 },
        })}
      />,
    );

    expect(screen.getByText("Cost")).toBeInTheDocument();
    expect(screen.getByText("$1.23 · 45.2k tok")).toBeInTheDocument();
  });

  it("omits the cost row when there is no cost", () => {
    render(<SessionInspector session={makeSession({ id: "insp-no-cost", cost: null })} />);
    expect(screen.queryByText("Cost")).not.toBeInTheDocument();
  });

  it("shows an empty state when there is no pull request", () => {
    render(<SessionInspector session={makeSession({ id: "insp-2", pr: null })} />);
    expect(screen.getByText("No pull request opened yet.")).toBeInTheDocument();
  });

  it("switches to the Changes view", () => {
    render(
      <SessionInspector
        session={makeSession({
          id: "insp-3",
          pr: makePR({ number: 56, additions: 18, deletions: 4, changedFiles: 3 }),
        })}
      />,
    );

    fireEvent.click(screen.getByRole("tab", { name: "Changes" }));
    expect(screen.getByText("Working tree")).toBeInTheDocument();
    expect(screen.getByText("3 files")).toBeInTheDocument();
  });

  it("switches to the Browser view placeholder", () => {
    render(<SessionInspector session={makeSession({ id: "insp-4" })} />);
    fireEvent.click(screen.getByRole("tab", { name: "Browser" }));
    expect(screen.getByText("No live browser preview.")).toBeInTheDocument();
  });
});
