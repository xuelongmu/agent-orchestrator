import type { BudgetConfig, OrchestratorConfig, Session } from "./types.js";

/**
 * A detected budget breach. `scope` says which cap was crossed, `limitUsd` is
 * the configured cap, and `actualUsd` is the observed cost that exceeded it.
 */
export interface BudgetBreach {
  scope: "session" | "project";
  limitUsd: number;
  actualUsd: number;
  /** Short, log-friendly evidence string (e.g. `budget_exceeded session $4.21 > $4.00`). */
  evidence: string;
}

/**
 * Resolve the effective budget caps for a project: per-project config wins over
 * the global default. A field left undefined falls back to the global default.
 *
 * Exception: a carried startup-only project (its `sourceConfigPath` is set, i.e.
 * its definition was merged in from a different config than the scope's global
 * top-level) does NOT inherit the merged scope's global `budget`. Its caps were
 * baked from its own startup config — if that config sets no cap, the project is
 * intentionally uncapped and must not be paused by an unrelated global budget.
 */
export function resolveBudget(config: OrchestratorConfig, projectId: string): BudgetConfig {
  const project = config.projects[projectId];
  const inheritGlobal = !project?.sourceConfigPath;
  return {
    perSessionUsd:
      project?.budget?.perSessionUsd ?? (inheritGlobal ? config.budget?.perSessionUsd : undefined),
    perProjectUsd:
      project?.budget?.perProjectUsd ?? (inheritGlobal ? config.budget?.perProjectUsd : undefined),
  };
}

/** A cap is active only when it is a positive number. */
function capActive(limit: number | undefined): limit is number {
  return typeof limit === "number" && limit > 0;
}

function formatUsd(value: number): string {
  return `$${value.toFixed(2)}`;
}

/**
 * Evaluate whether a session has crossed any configured budget cap.
 *
 * The per-session cap compares against the session's own estimated cost; the
 * per-project cap compares against `projectCostUsd` — the combined estimated
 * cost of all sessions in the project, computed by the caller. Returns null
 * when no cap is configured or none is exceeded.
 */
export function evaluateBudgetBreach(
  config: OrchestratorConfig,
  session: Session,
  projectCostUsd: number,
): BudgetBreach | null {
  const budget = resolveBudget(config, session.projectId);
  const sessionCost = session.agentInfo?.cost?.estimatedCostUsd ?? 0;

  if (capActive(budget.perSessionUsd) && sessionCost > budget.perSessionUsd) {
    return {
      scope: "session",
      limitUsd: budget.perSessionUsd,
      actualUsd: sessionCost,
      evidence: `budget_exceeded session ${formatUsd(sessionCost)} > ${formatUsd(budget.perSessionUsd)}`,
    };
  }

  if (capActive(budget.perProjectUsd) && projectCostUsd > budget.perProjectUsd) {
    return {
      scope: "project",
      limitUsd: budget.perProjectUsd,
      actualUsd: projectCostUsd,
      evidence: `budget_exceeded project ${formatUsd(projectCostUsd)} > ${formatUsd(budget.perProjectUsd)}`,
    };
  }

  return null;
}
