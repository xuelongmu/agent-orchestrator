/**
 * Report Watcher — Background trigger system for agent reports (#140)
 *
 * Monitors agent reports and triggers actions when anomalies are detected:
 * - No acknowledge timeout: agent didn't acknowledge task after spawn
 * - Stale report: agent hasn't reported in a while
 * - Agent blocked: agent reported blocked/needs_input state
 *
 * This module is designed to be called from the lifecycle-manager polling loop.
 */

import type { Session, SessionStatus } from "./types.js";
import { isAgentReportFresh, readAgentReport, type AgentReport } from "./agent-report.js";

/** Reported states that park a session on a human decision. */
const DECISION_REPORT_STATES: readonly string[] = ["needs_input", "needs_decision"];

/** True when a reported state is one that parks the session on a human decision. */
export function isDecisionReportState(state: string): boolean {
  return DECISION_REPORT_STATES.includes(state);
}

/** True when a report is one that parks the session on a human decision. */
export function isDecisionReport(report: AgentReport | null): boolean {
  return !!report && isDecisionReportState(report.state);
}

/**
 * Whether a decision report (`needs_input` or `needs_decision`) is an ACTIVE
 * block (#12). Active while EITHER the session is currently parked in
 * needs_input, OR the report is still fresh — so a fresh decision request stays
 * active even when PR-enrichment ordering leaves the derived state as
 * pr_open/mergeable, while a decision resolved by inference (agent resumed
 * working, report gone stale) is no longer active and stops being exempted from
 * stale-report handling.
 *
 * This is the single activity rule for decision reports: the notify-callback
 * decision instance (#13) binds its identity to it, so an identity exists only
 * while the decision that minted it is still live.
 */
export function isDecisionReportActive(
  session: Session,
  report: AgentReport | null,
  now: Date = new Date(),
): boolean {
  if (!isDecisionReport(report) || !report) return false;
  return session.lifecycle?.session.state === "needs_input" || isAgentReportFresh(report, now);
}

/**
 * Whether a `needs_decision` report specifically is an active block (#12) — the
 * decision-only narrowing of {@link isDecisionReportActive}.
 */
export function isNeedsDecisionActive(
  session: Session,
  report: AgentReport | null,
  now: Date = new Date(),
): boolean {
  if (report?.state !== "needs_decision") return false;
  return isDecisionReportActive(session, report, now);
}

/**
 * Report watcher trigger types.
 */
export type ReportWatcherTrigger =
  | "no_acknowledge"
  | "stale_report"
  | "agent_needs_input";

/**
 * Result of a report audit check.
 */
export interface ReportAuditResult {
  /** Which trigger was activated, if any. */
  trigger: ReportWatcherTrigger | null;
  /** Human-readable description of the finding. */
  message: string;
  /** ISO timestamp when the check was performed. */
  checkedAt: string;
  /** The agent report that was checked (if available). */
  report: AgentReport | null;
  /** Time since spawn in milliseconds. */
  timeSinceSpawnMs?: number;
  /** Time since last report in milliseconds. */
  timeSinceReportMs?: number;
}

/**
 * Configuration for report watcher thresholds.
 */
export interface ReportWatcherConfig {
  /** Time after spawn before triggering no-acknowledge (default: 10 minutes). */
  acknowledgeTimeoutMs: number;
  /** Time without reports before triggering stale report (default: 30 minutes). */
  staleReportTimeoutMs: number;
  /** Whether to check for acknowledge timeout. */
  checkAcknowledge: boolean;
  /** Whether to check for stale reports. */
  checkStale: boolean;
  /** Whether to check for blocked agents. */
  checkBlocked: boolean;
}

/**
 * Default report watcher configuration.
 */
export const DEFAULT_REPORT_WATCHER_CONFIG: ReportWatcherConfig = {
  acknowledgeTimeoutMs: 10 * 60 * 1000, // 10 minutes
  staleReportTimeoutMs: 30 * 60 * 1000, // 30 minutes
  checkAcknowledge: true,
  checkStale: true,
  checkBlocked: true,
};

/**
 * Terminal statuses that should not be audited.
 */
const TERMINAL_STATUSES: Set<SessionStatus> = new Set([
  "done",
  "terminated",
  "killed",
  "cleanup",
  "merged",
  "errored",
]);

/**
 * Check if a session should be audited for report issues.
 * Note: spawning sessions are included so acknowledge timeout can fire.
 */
export function shouldAuditSession(session: Session): boolean {
  // Skip terminal sessions
  if (TERMINAL_STATUSES.has(session.status)) {
    return false;
  }
  // Skip orchestrator sessions
  if (session.lifecycle?.session.kind === "orchestrator") {
    return false;
  }
  // Note: spawning sessions are NOT skipped — they need acknowledge timeout checks
  return true;
}

/**
 * Check for acknowledge timeout: agent didn't acknowledge task within threshold.
 */
export function checkAcknowledgeTimeout(
  session: Session,
  report: AgentReport | null,
  now: Date,
  config: ReportWatcherConfig,
): ReportAuditResult | null {
  if (!config.checkAcknowledge) return null;

  // If we have any report (acknowledge or otherwise), the agent responded
  if (report) return null;

  // Check time since spawn
  const createdAt = session.createdAt ?? session.metadata["createdAt"];
  if (!createdAt) return null;

  const spawnTime = typeof createdAt === "string" ? Date.parse(createdAt) : createdAt.getTime();
  if (Number.isNaN(spawnTime)) return null;

  const timeSinceSpawn = now.getTime() - spawnTime;
  if (timeSinceSpawn < config.acknowledgeTimeoutMs) return null;

  return {
    trigger: "no_acknowledge",
    message: `Agent has not acknowledged task after ${Math.round(timeSinceSpawn / 60000)} minutes`,
    checkedAt: now.toISOString(),
    report: null,
    timeSinceSpawnMs: timeSinceSpawn,
  };
}

/**
 * Check for stale report: agent hasn't reported in a while.
 */
export function checkStaleReport(
  session: Session,
  report: AgentReport | null,
  now: Date,
  config: ReportWatcherConfig,
): ReportAuditResult | null {
  if (!config.checkStale) return null;

  // If no report, the acknowledge check handles it
  if (!report) return null;

  // Check if the report is stale
  const reportTime = Date.parse(report.timestamp);
  if (Number.isNaN(reportTime)) return null;

  const timeSinceReport = now.getTime() - reportTime;
  if (timeSinceReport < config.staleReportTimeoutMs) return null;

  // Don't flag as stale if the agent is in a waiting state (that's expected).
  // needs_decision is exempt ONLY while it is an ACTIVE block — once resolved by
  // inference, a stale needs_decision must NOT shield a working agent forever (#12).
  if (report.state === "waiting" || report.state === "needs_input") return null;
  if (isNeedsDecisionActive(session, report, now)) return null;

  return {
    trigger: "stale_report",
    message: `Agent report is stale (${Math.round(timeSinceReport / 60000)} minutes since last report)`,
    checkedAt: now.toISOString(),
    report,
    timeSinceReportMs: timeSinceReport,
  };
}

/**
 * Check for blocked agent: agent reported blocked or needs_input state.
 */
export function checkBlockedAgent(
  session: Session,
  report: AgentReport | null,
  now: Date,
  config: ReportWatcherConfig,
): ReportAuditResult | null {
  if (!config.checkBlocked) return null;

  // If no report, nothing to check
  if (!report) return null;

  // A needs_decision report is an active block only while active (in needs_input OR
  // still fresh). A decision resolved by inference — reported state lingers but the
  // agent has resumed working — must NOT keep this trigger active (so
  // auditAgentReports reaches the stale-report check); a FRESH decision on a session
  // whose derived state is pr_open/mergeable (PR-enrichment ordering) must stay
  // active so the human is still notified (#12).
  if (report.state === "needs_decision" && !isNeedsDecisionActive(session, report, now)) {
    return null;
  }

  if (report.state === "needs_input" || report.state === "needs_decision") {
    const reportTime = Date.parse(report.timestamp);
    const timeSinceReportMs = Number.isNaN(reportTime) ? undefined : now.getTime() - reportTime;
    // needs_decision carries an explicit confidence + question (#12); surface
    // both so the notification hands the human the full judgment call.
    const message =
      report.state === "needs_decision"
        ? `Agent needs a decision${
            report.confidence !== undefined
              ? ` (confidence ${Math.round(report.confidence * 100)}%)`
              : ""
          }: ${report.question ?? report.note ?? "waiting for user decision"}`
        : `Agent needs input: ${report.note ?? "waiting for user decision"}`;
    return {
      trigger: "agent_needs_input",
      message,
      checkedAt: now.toISOString(),
      report,
      timeSinceReportMs,
    };
  }

  // Note: "blocked" is not in the current AGENT_REPORTED_STATES but we check for it
  // in case it gets added or for forward compatibility
  return null;
}

/**
 * Audit a session's agent reports for issues.
 * Returns the first triggered audit result, or null if no issues found.
 */
export function auditAgentReports(
  session: Session,
  config: Partial<ReportWatcherConfig> = {},
  now: Date = new Date(),
): ReportAuditResult | null {
  if (!shouldAuditSession(session)) {
    return null;
  }

  const fullConfig: ReportWatcherConfig = {
    ...DEFAULT_REPORT_WATCHER_CONFIG,
    ...config,
  };

  const report = readAgentReport(session.metadata);

  // Check in priority order: blocked > no_acknowledge > stale
  const blockedResult = checkBlockedAgent(session, report, now, fullConfig);
  if (blockedResult) return blockedResult;

  const acknowledgeResult = checkAcknowledgeTimeout(session, report, now, fullConfig);
  if (acknowledgeResult) return acknowledgeResult;

  const staleResult = checkStaleReport(session, report, now, fullConfig);
  if (staleResult) return staleResult;

  return null;
}

/**
 * Get the reaction key for a report watcher trigger.
 * Used to look up the reaction configuration.
 */
export function getReactionKeyForTrigger(trigger: ReportWatcherTrigger): string {
  switch (trigger) {
    case "no_acknowledge":
      return "report-no-acknowledge";
    case "stale_report":
      return "report-stale";
    case "agent_needs_input":
      return "report-needs-input";
  }
}

/**
 * Metadata keys for storing report watcher flags.
 */
export const REPORT_WATCHER_METADATA_KEYS = {
  /** ISO timestamp of last audit */
  LAST_AUDITED_AT: "reportWatcherLastAuditedAt",
  /** Current active trigger (if any) */
  ACTIVE_TRIGGER: "reportWatcherActiveTrigger",
  /** ISO timestamp when trigger was first activated */
  TRIGGER_ACTIVATED_AT: "reportWatcherTriggerActivatedAt",
  /** Number of times this trigger has fired */
  TRIGGER_COUNT: "reportWatcherTriggerCount",
} as const;
