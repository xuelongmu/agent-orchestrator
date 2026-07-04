/**
 * Lifecycle Manager — state machine + polling loop + reaction engine.
 *
 * Periodically polls all sessions and:
 * 1. Detects state transitions (spawning → working → pr_open → etc.)
 * 2. Emits events on transitions
 * 3. Triggers reactions (auto-handle CI failures, review comments, etc.)
 * 4. Escalates to human notification when auto-handling fails
 *
 * Reference: scripts/claude-session-status, scripts/claude-review-check
 */

import { randomUUID } from "node:crypto";
import { readFile, stat } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";
import { recordActivityEvent } from "./activity-events.js";
import {
  ACTIVITY_STATE,
  SESSION_STATUS,
  TERMINAL_STATUSES,
  type ActivityState,
  type LifecycleManager,
  type OpenCodeSessionManager,
  type SessionId,
  type SessionStatus,
  type EventType,
  type OrchestratorEvent,
  type OrchestratorConfig,
  type ReactionConfig,
  type ReactionResult,
  type PluginRegistry,
  type Runtime,
  type Agent,
  type SCM,
  type Notifier,
  type Session,
  type CanonicalSessionLifecycle,
  type EventPriority,
  type ProjectConfig as _ProjectConfig,
  type PREnrichmentData,
  type CICheck,
  type CIFailureSummary,
  type PRInfo,
  type PRRetargetOutcome,
  type ReviewComment,
  type ReviewSummary,
  type ProcessProbeResult,
  isProcessProbeIndeterminate,
} from "./types.js";
import {
  buildLifecycleMetadataPatch,
  cloneLifecycle,
  deriveLegacyStatus,
  isBlockedByDependency,
} from "./lifecycle-state.js";
import {
  collectSatisfiedDependencies,
  computeRemainingBlockedBy,
  countActiveSessions,
} from "./dependency-scheduler.js";
import { updateMetadata } from "./metadata.js";
import { resolveStackedChildBase } from "./stacked.js";
import { getProjectSessionsDir } from "./paths.js";
import { applyDecisionToLifecycle as commitLifecycleDecisionInPlace } from "./lifecycle-transition.js";
import {
  classifyActivitySignal,
  createActivitySignal,
  formatActivitySignalEvidence,
  hasPositiveIdleEvidence,
  isWeakActivityEvidence,
} from "./activity-signal.js";
import { isAgentReportFresh, mapAgentReportToLifecycle, readAgentReport } from "./agent-report.js";
import {
  computeConfidence,
  summarizeConfidenceFactors,
  type ConfidenceAssessment,
  type ConfidenceSignals,
} from "./confidence.js";
import { createCodeReviewStore, type CodeReviewSeverity } from "./code-review-store.js";
import { evaluateBudgetBreach, resolveBudget } from "./budget.js";
import {
  auditAgentReports,
  getReactionKeyForTrigger,
  REPORT_WATCHER_METADATA_KEYS,
} from "./report-watcher.js";
import { createCorrelationId, createProjectObserver } from "./observability.js";
import { resolveNotifierTarget } from "./notifier-resolution.js";
import { recordNotificationDelivery } from "./notification-observability.js";
import { resolveSessionRole } from "./agent-selection.js";
import {
  DETECTING_MAX_ATTEMPTS,
  createDetectingDecision,
  isDetectingTimedOut,
  parseAttemptCount,
  resolvePREnrichmentDecision,
  resolvePRLiveDecision,
  resolveProbeDecision,
  type LifecycleDecision,
} from "./lifecycle-status-decisions.js";
import { dedupePrInfos } from "./utils/pr.js";
import {
  buildCIFailureNotificationData,
  buildPRStateNotificationData,
  buildReactionEscalationNotificationData,
  buildReactionNotificationData,
  buildSessionTransitionNotificationData,
  type NotificationEventContext,
} from "./notification-data.js";

/** Parse a duration string like "10m", "30s", "1h" to milliseconds. */
function parseDuration(str: string): number {
  const match = str.match(/^(\d+)(s|m|h)$/);
  if (!match) return 0;
  const value = parseInt(match[1], 10);
  switch (match[2]) {
    case "s":
      return value * 1000;
    case "m":
      return value * 60_000;
    case "h":
      return value * 3_600_000;
    default:
      return 0;
  }
}

/** Severity ordering for picking the riskiest open review finding. */
const CODE_REVIEW_SEVERITY_RANK: Record<CodeReviewSeverity, number> = {
  info: 1,
  warning: 2,
  error: 3,
};

/** The agent's explicit judgment call, when the current report is `needs_decision`. */
interface NeedsDecisionContext {
  confidence?: number;
  question: string;
}

/** Tolerance (ms) when matching a needs_decision report to the current block. */
const NEEDS_DECISION_ENTRY_TOLERANCE_MS = 2_000;

/**
 * Read the `needs_decision` report backing the session's CURRENT needs_input
 * block, else null. `needs_decision` reports are sticky (exempt from
 * stale-report handling until a later `ao report` clears them), so a wall-clock
 * freshness window would wrongly drop the context for a genuinely-still-blocked
 * session (e.g. a delayed poll or daemon restart). Instead we anchor to the
 * session's last transition: applyAgentReport stamps the report time AND
 * lastTransitionAt together, so a report predating the current state entry
 * belongs to a prior, resolved block (lifecycle inference left and re-entered
 * needs_input) and must not leak into an unrelated later needs-input (#12).
 */
function readNeedsDecisionReport(session: Session): NeedsDecisionContext | null {
  const report = readAgentReport(session.metadata);
  if (report?.state !== "needs_decision" || !report.question) return null;
  const reportedAt = Date.parse(report.timestamp);
  const enteredAtRaw = session.lifecycle.session.lastTransitionAt;
  const enteredAt = enteredAtRaw ? Date.parse(enteredAtRaw) : NaN;
  if (
    Number.isFinite(reportedAt) &&
    Number.isFinite(enteredAt) &&
    reportedAt < enteredAt - NEEDS_DECISION_ENTRY_TOLERANCE_MS
  ) {
    return null;
  }
  return { confidence: report.confidence, question: report.question };
}

/** Human-facing notification message for an agent-initiated decision (#12). */
function formatNeedsDecisionMessage(decision: NeedsDecisionContext): string {
  const confidence =
    decision.confidence !== undefined
      ? ` (confidence ${Math.round(decision.confidence * 100)}%)`
      : "";
  return `Decision needed${confidence}: ${decision.question}`;
}

/** Compose the human-facing question when a low-confidence action is held (#12). */
function buildConfidenceEscalationQuestion(
  reactionKey: string,
  action: string,
  threshold: number,
  assessment: ConfidenceAssessment,
): string {
  const pct = Math.round(assessment.score * 100);
  const thresholdPct = Math.round(threshold * 100);
  return (
    `Confidence ${pct}% is below the ${thresholdPct}% threshold to auto-run '${action}' ` +
    `for '${reactionKey}'. Reasons: ${summarizeConfidenceFactors(assessment)}. ` +
    `Proceed manually or intervene?`
  );
}

/** Reaction keys for conditions that can oscillate (e.g. CI failing→pending→failing).
 *  Their trackers survive status exit so the escalation budget accumulates
 *  across oscillations instead of resetting to zero each time.
 *  Note: "merge-conflicts" is NOT here — statusToEventType never emits
 *  "merge.conflicts", so the transition handler at line ~1892 can't reach it.
 *  Merge-conflict tracker lifecycle is managed in maybeDispatchMergeConflicts. */
const PERSISTENT_REACTION_KEYS = new Set(["ci-failed"]);

/** Number of consecutive CI-passing polls required before the ci-failed tracker
 *  (including its escalated flag) is cleared, allowing a fresh budget for the
 *  next real CI failure incident. */
const CI_PASSING_STABLE_THRESHOLD = 2;

type TransitionReaction = {
  key: string;
  result: ReactionResult | null;
  messageEnriched?: boolean;
};

type WorkspaceBranchProbe =
  | { kind: "branch"; branch: string }
  | { kind: "detached" }
  | { kind: "unavailable" };

const TRANSIENT_DETACHED_GIT_MARKERS = [
  "rebase-merge",
  "rebase-apply",
  "CHERRY_PICK_HEAD",
  "BISECT_LOG",
] as const;

function isErrnoException(error: unknown): error is NodeJS.ErrnoException {
  return typeof error === "object" && error !== null && "code" in error;
}

async function pathExists(path: string): Promise<boolean> {
  try {
    await stat(path);
    return true;
  } catch (error) {
    if (isErrnoException(error) && error.code === "ENOENT") {
      return false;
    }
    throw error;
  }
}

async function hasTransientDetachedGitState(gitDir: string): Promise<boolean> {
  const checks = await Promise.all(
    TRANSIENT_DETACHED_GIT_MARKERS.map((marker) => pathExists(join(gitDir, marker))),
  );
  return checks.some(Boolean);
}

async function resolveGitDir(workspacePath: string): Promise<string> {
  const dotGitPath = join(workspacePath, ".git");
  const dotGitStats = await stat(dotGitPath);
  if (dotGitStats.isDirectory()) return dotGitPath;

  const dotGitContent = (await readFile(dotGitPath, "utf8")).trim();
  const gitDirMatch = dotGitContent.match(/^gitdir:\s*(.+)$/i);
  if (!gitDirMatch) {
    throw new Error(`Invalid .git pointer in workspace: ${workspacePath}`);
  }

  return resolve(dirname(dotGitPath), gitDirMatch[1].trim());
}

async function readWorkspaceBranch(workspacePath: string): Promise<WorkspaceBranchProbe> {
  let gitDir: string;
  try {
    gitDir = await resolveGitDir(workspacePath);
  } catch {
    return { kind: "unavailable" };
  }

  try {
    const head = (await readFile(join(gitDir, "HEAD"), "utf8")).trim();
    const prefix = "ref: refs/heads/";
    if (!head.startsWith(prefix)) {
      return (await hasTransientDetachedGitState(gitDir))
        ? { kind: "unavailable" }
        : { kind: "detached" };
    }

    const branch = head.slice(prefix.length).trim();
    if (branch.length > 0) {
      return { kind: "branch", branch };
    }
    return (await hasTransientDetachedGitState(gitDir))
      ? { kind: "unavailable" }
      : { kind: "detached" };
  } catch {
    return { kind: "unavailable" };
  }
}

/** Infer a reasonable priority from event type. */
function inferPriority(type: EventType): EventPriority {
  if (type.includes("stuck") || type.includes("needs_input") || type.includes("errored")) {
    return "urgent";
  }
  if (type.startsWith("summary.")) {
    return "info";
  }
  if (
    type.includes("approved") ||
    type.includes("ready") ||
    type.includes("merged") ||
    type.includes("completed")
  ) {
    return "action";
  }
  if (type.includes("fail") || type.includes("changes_requested") || type.includes("conflicts")) {
    return "warning";
  }
  return "info";
}

/** Create an OrchestratorEvent with defaults filled in. */
function createEvent(
  type: EventType,
  opts: {
    sessionId: SessionId;
    projectId: string;
    message: string;
    priority?: EventPriority;
    data?: Record<string, unknown>;
  },
): OrchestratorEvent {
  return {
    id: randomUUID(),
    type,
    priority: opts.priority ?? inferPriority(type),
    sessionId: opts.sessionId,
    projectId: opts.projectId,
    timestamp: new Date(),
    message: opts.message,
    data: opts.data ?? {},
  };
}

/** Determine which event type corresponds to a status transition. */
function statusToEventType(_from: SessionStatus | undefined, to: SessionStatus): EventType | null {
  switch (to) {
    case "working":
      return "session.working";
    case "pr_open":
      return "pr.created";
    case "ci_failed":
      return "ci.failing";
    case "review_pending":
      return "review.pending";
    case "changes_requested":
      return "review.changes_requested";
    case "approved":
      return "review.approved";
    case "mergeable":
      return "merge.ready";
    case "merged":
      return "merge.completed";
    case "needs_input":
      return "session.needs_input";
    case "stuck":
      return "session.stuck";
    case "errored":
      return "session.errored";
    case "killed":
      return "session.killed";
    default:
      return null;
  }
}

function prStateToEventType(
  from: Session["lifecycle"]["pr"]["state"],
  to: Session["lifecycle"]["pr"]["state"],
): EventType | null {
  if (from === to) return null;
  switch (to) {
    case "closed":
      return "pr.closed";
    default:
      return null;
  }
}

/** PR context for event enrichment. */
type EventContext = NotificationEventContext;

/**
 * Minimal session context required for reaction execution and event enrichment.
 * Used for system-level events (like all-complete) that don't have a real session.
 */
interface ReactionSessionContext {
  id: SessionId;
  projectId: string;
  pr: Session["pr"];
  issueId: string | null;
  branch: string | null;
  metadata: Record<string, string>;
  agentInfo: Session["agentInfo"];
}

/**
 * Build event context with PR and issue information for webhook payloads.
 * This enriches events with useful metadata so external consumers (Telegram, Discord, etc.)
 * can display meaningful information without making additional API calls.
 */
function buildEventContext(
  session: Session | ReactionSessionContext,
  prEnrichmentCache: Map<string, PREnrichmentData>,
): EventContext {
  const sessionPRs = dedupePrInfos(
    "prs" in session && Array.isArray(session.prs) ? session.prs : session.pr ? [session.pr] : [],
  );

  const prs: EventContext["prs"] = sessionPRs.map((p) => {
    const cached = prEnrichmentCache.get(`${p.owner}/${p.repo}#${p.number}`);
    return {
      url: p.url,
      title: cached?.title ?? null,
      number: p.number,
      branch: p.branch,
      baseBranch: p.baseBranch,
      owner: p.owner,
      repo: p.repo,
      isDraft: p.isDraft,
    };
  });

  const pr = prs[0] ?? null;

  return {
    pr,
    prs,
    issueId: session.issueId,
    issueTitle: session.metadata["issueTitle"] ?? null,
    summary: session.agentInfo?.summary ?? null,
    branch: session.branch,
  };
}

/** Map event type to reaction config key. */
function eventToReactionKey(eventType: EventType): string | null {
  switch (eventType) {
    case "pr.closed":
      return "pr-closed";
    case "ci.failing":
      return "ci-failed";
    case "review.changes_requested":
      return "changes-requested";
    case "automated_review.found":
      return "bugbot-comments";
    case "merge.conflicts":
      return "merge-conflicts";
    case "merge.ready":
      return "approved-and-green";
    case "merge.completed":
      // Fires when a PR actually merges (status → merged), unlike
      // `approved-and-green` which fires on merge.ready before the merge lands.
      // Lets a `spawn-session` reaction launch dependents on the real merge (#10).
      return "pr-merged";
    case "session.stuck":
      return "agent-stuck";
    case "session.needs_input":
      return "agent-needs-input";
    case "session.killed":
      return "agent-exited";
    case "summary.all_complete":
      return "all-complete";
    default:
      return null;
  }
}

function transitionLogLevel(status: SessionStatus): "info" | "warn" | "error" {
  const eventType = statusToEventType(undefined, status);
  if (!eventType) {
    return "info";
  }
  const priority = inferPriority(eventType);
  if (priority === "urgent") {
    return "error";
  }
  if (priority === "warning") {
    return "warn";
  }
  return "info";
}

interface DeterminedStatus {
  status: SessionStatus;
  evidence: string;
  detectingAttempts: number;
  /** True when probes produced no reliable verdict and lifecycle metadata must remain untouched. */
  skipMetadataWrite?: boolean;
  /** ISO timestamp when detecting first started. */
  detectingStartedAt?: string;
  /** Hash of evidence for unchanged-evidence detection. */
  detectingEvidenceHash?: string;
}

interface ProbeResult {
  state: "alive" | "dead" | "unknown";
  failed: boolean;
  indeterminate?: boolean;
}

function processProbeResultToProbeResult(result: ProcessProbeResult): ProbeResult {
  if (isProcessProbeIndeterminate(result)) {
    return { state: "unknown", failed: false, indeterminate: true };
  }
  return { state: result ? "alive" : "dead", failed: false };
}

function splitEvidenceSignals(evidence: string): string[] {
  return evidence
    .split(/\s+/)
    .map((signal) => signal.trim())
    .filter((signal) => signal.length > 0);
}

function primaryLifecycleReason(lifecycle: CanonicalSessionLifecycle): string {
  if (lifecycle.session.state === "detecting") return lifecycle.session.reason;
  if (lifecycle.pr.reason !== "not_created" && lifecycle.pr.reason !== "in_progress") {
    return lifecycle.pr.reason;
  }
  if (lifecycle.runtime.reason !== "process_running") {
    return lifecycle.runtime.reason;
  }
  return lifecycle.session.reason;
}

function buildTransitionObservabilityData(
  previous: CanonicalSessionLifecycle,
  next: CanonicalSessionLifecycle,
  oldStatus: SessionStatus,
  newStatus: SessionStatus,
  evidence: string,
  detectingAttempts: number,
  statusTransition: boolean,
  reaction?: { key: string; result: ReactionResult | null },
): Record<string, unknown> {
  return {
    oldStatus,
    newStatus,
    statusTransition,
    previousSessionState: previous.session.state,
    newSessionState: next.session.state,
    previousSessionReason: previous.session.reason,
    newSessionReason: next.session.reason,
    previousPRState: previous.pr.state,
    newPRState: next.pr.state,
    previousPRReason: previous.pr.reason,
    newPRReason: next.pr.reason,
    previousRuntimeState: previous.runtime.state,
    newRuntimeState: next.runtime.state,
    previousRuntimeReason: previous.runtime.reason,
    newRuntimeReason: next.runtime.reason,
    primaryReason: primaryLifecycleReason(next),
    evidence,
    signalsConsulted: splitEvidenceSignals(evidence),
    detectingAttempts,
    recoveryAction: reaction?.result?.action ?? null,
    reactionKey: reaction?.key ?? null,
    reactionSuccess: reaction?.result?.success ?? null,
    escalated: reaction?.result?.escalated ?? null,
  };
}

export interface LifecycleManagerDeps {
  config: OrchestratorConfig;
  registry: PluginRegistry;
  sessionManager: OpenCodeSessionManager;
  /** When set, only poll sessions belonging to this project. */
  projectId?: string;
}

/** Track attempt counts for reactions per session. */
interface ReactionTracker {
  attempts: number;
  firstTriggered: Date;
  /** True after this reaction has escalated. Short-circuits further dispatches
   *  until the underlying condition resolves and the tracker is explicitly cleared. */
  escalated?: boolean;
}

/** Create a LifecycleManager instance. */
export function createLifecycleManager(deps: LifecycleManagerDeps): LifecycleManager {
  const { config, registry, sessionManager, projectId: scopedProjectId } = deps;
  const observer = createProjectObserver(config, "lifecycle-manager");

  const states = new Map<SessionId, SessionStatus>();
  const activityStateCache = new Map<string, ActivityState>(); // sessionId → last observed activity
  const reactionTrackers = new Map<string, ReactionTracker>(); // "sessionId:reactionKey"
  let pollTimer: ReturnType<typeof setInterval> | null = null;
  let polling = false; // re-entrancy guard
  let allCompleteEmitted = false; // guard against repeated all_complete
  const branchAdoptionReservations = new Map<string, SessionId>();

  /**
   * Cache for PR enrichment data within a single poll cycle.
   * Cleared at the start of each pollAll() call.
   * Key format: "${owner}/${repo}#${number}"
   */
  const prEnrichmentCache = new Map<string, PREnrichmentData>();

  function normalizeSessionPRs(session: Session): PRInfo[] {
    const candidatePRs = session.prs.length > 0 ? session.prs : session.pr ? [session.pr] : [];
    const uniquePRs = dedupePrInfos(candidatePRs);
    if (uniquePRs.length !== session.prs.length || session.pr !== (uniquePRs[0] ?? null)) {
      session.prs = uniquePRs;
      session.pr = uniquePRs[0] ?? null;
    }
    return uniquePRs;
  }

  function indexedPRMetadataCleanup(
    session: Session,
    prCount: number,
  ): Partial<Record<string, string>> {
    const updates: Partial<Record<string, string>> = {};
    for (const key of Object.keys(session.metadata)) {
      const match = key.match(/^(prEnrichment|prReviewComments)_(\d+)$/);
      if (!match) continue;
      const index = Number.parseInt(match[2], 10);
      if (Number.isNaN(index) || index >= prCount) {
        updates[key] = "";
      }
    }
    return updates;
  }

  function getPREnrichmentForSession(
    session: Session | ReactionSessionContext,
  ): PREnrichmentData | undefined {
    if (!session.pr) return undefined;
    return prEnrichmentCache.get(`${session.pr.owner}/${session.pr.repo}#${session.pr.number}`);
  }

  /** Repos where Guard 1 returned 304 in the current poll — safe to skip detectPR. */
  let prListUnchangedRepos = new Set<string>();

  /**
   * Per-session timestamp of last review backlog API check.
   * Used to throttle review thread checks to at most once per 2 minutes.
   * In-memory only — resets on restart (acceptable since it's a rate-limit hint, not state).
   */
  const lastReviewBacklogCheckAt = new Map<SessionId, number>();

  /** Throttle interval for review backlog API calls (2 minutes). */
  const REVIEW_BACKLOG_THROTTLE_MS = 2 * 60 * 1000;

  /**
   * Default cap on bot review→fix rounds (Codex PR-comment loop) before the
   * loop stops auto-dispatching and escalates to a human. Overridable per
   * project/reaction via the `bugbot-comments` reaction's `maxRounds`.
   */
  const DEFAULT_MAX_REVIEW_ROUNDS = 5;

  /**
   * Default cap on automatic nudges re-delivering unaddressed PR review comments
   * to a stuck/idle agent before escalating to a human (needs_input + notify).
   * Overridable per project/reaction via the `agent-stuck` reaction's
   * `nudgeRetries`.
   */
  const DEFAULT_STUCK_NUDGE_RETRIES = 3;

  /**
   * Populate the PR enrichment cache using batch GraphQL queries.
   * This is called once per poll cycle to fetch data for all PRs efficiently.
   */
  async function populatePREnrichmentCache(sessions: Session[]): Promise<void> {
    // Clear previous cache
    prEnrichmentCache.clear();
    prListUnchangedRepos = new Set();

    // Collect all unique PRs and repos keyed by their owning session's project/plugin.
    // Repos are collected from ALL sessions (not just ones with PRs) so Guard 1 runs
    // for every active repo — enabling detectPR gating even when no PRs exist yet.
    const prsByPlugin = new Map<string, Array<NonNullable<Session["pr"]>>>();
    const reposByPlugin = new Map<string, Set<string>>();
    const seenPRKeys = new Set<string>();
    for (const session of sessions) {
      const project = config.projects[session.projectId];
      if (!project?.scm?.plugin || !project.repo) continue;

      const pluginKey = project.scm.plugin;
      if (!prsByPlugin.has(pluginKey)) {
        prsByPlugin.set(pluginKey, []);
      }
      if (!reposByPlugin.has(pluginKey)) {
        reposByPlugin.set(pluginKey, new Set());
      }
      reposByPlugin.get(pluginKey)!.add(project.repo);
      const sessionPRs = normalizeSessionPRs(session);
      if (sessionPRs.length === 0) continue;
      // Loop over all PRs in the session — supports multi-repo sessions
      // where an agent opened PRs on multiple repos.
      for (const pr of sessionPRs) {
        const actualPRRepo = `${pr.owner}/${pr.repo}`;
        if (actualPRRepo !== project.repo) {
          reposByPlugin.get(pluginKey)!.add(actualPRRepo);
        }
        const prKey = `${pr.owner}/${pr.repo}#${pr.number}`;
        if (seenPRKeys.has(prKey)) continue;
        seenPRKeys.add(prKey);
        const pluginPRs = prsByPlugin.get(pluginKey);
        if (pluginPRs) {
          pluginPRs.push(pr);
        }
      }
    }

    // Fetch enrichment data and run Guard 1 for all active repos
    for (const [pluginKey, pluginPRs] of prsByPlugin) {
      const scm = registry.get<SCM>("scm", pluginKey);
      if (!scm?.enrichSessionsPRBatch) continue;

      const pluginRepos = [...(reposByPlugin.get(pluginKey) ?? [])];
      const batchStartTime = Date.now();
      try {
        const enrichmentData = await scm.enrichSessionsPRBatch(
          pluginPRs,
          {
            recordSuccess(_data) {
              const batchDuration = Date.now() - batchStartTime;
              observer?.recordOperation({
                metric: "graphql_batch",
                operation: "batch_enrichment",
                correlationId: createCorrelationId("graphql-batch"),
                outcome: "success",
                projectId: scopedProjectId,
                durationMs: batchDuration,
                data: {
                  plugin: pluginKey,
                  prCount: pluginPRs.length,
                  prKeys: pluginPRs.map((pr) => `${pr.owner}/${pr.repo}#${pr.number}`),
                },
                level: "info",
              });
            },
            recordFailure(data) {
              const batchDuration = Date.now() - batchStartTime;
              observer?.recordOperation({
                metric: "graphql_batch",
                operation: "batch_enrichment",
                correlationId: createCorrelationId("graphql-batch"),
                outcome: "failure",
                reason: data.error,
                level: "warn",
                data: {
                  plugin: pluginKey,
                  prCount: pluginPRs.length,
                  error: data.error,
                  durationMs: batchDuration,
                },
              });
            },
            log(level, message) {
              observer?.recordDiagnostic?.({
                operation: "batch_enrichment.log",
                correlationId: createCorrelationId("graphql-batch"),
                projectId: scopedProjectId,
                message,
                level,
                data: {
                  plugin: pluginKey,
                  source: "ao-graphql-batch",
                },
              });
            },
            reportPRListUnchangedRepos(repos) {
              for (const repo of repos) {
                prListUnchangedRepos.add(repo);
              }
            },
          },
          pluginRepos,
        );

        // Merge into cache
        for (const [key, data] of enrichmentData) {
          prEnrichmentCache.set(key, data);
        }
      } catch (err) {
        // Batch fetch failed - individual calls will still work
        const errorMsg = err instanceof Error ? err.message : String(err);
        const batchCorrelationId = createCorrelationId("batch-enrichment");
        observer?.recordOperation?.({
          metric: "lifecycle_poll",
          operation: "batch_enrichment",
          correlationId: batchCorrelationId,
          outcome: "failure",
          reason: errorMsg,
          level: "warn",
          data: { plugin: pluginKey, prCount: pluginPRs.length },
        });
        recordActivityEvent({
          // Tag with scopedProjectId when the lifecycle worker is project-scoped
          // so `ao events list --project <id>` surfaces this failure. Unscoped
          // (multi-project) supervisors leave projectId null because the batch
          // crosses project boundaries — RCA there should query without --project.
          projectId: scopedProjectId,
          source: "scm",
          kind: "scm.batch_enrich_failed",
          level: "warn",
          summary: `batch_enrich failed for ${pluginPRs.length} PR(s)`,
          data: {
            plugin: pluginKey,
            prCount: pluginPRs.length,
            errorMessage: errorMsg,
          },
        });
      }
    }

    // Discover PRs for sessions that don't have one yet.
    // Only run detectPR when Guard 1 returned 200 (repo's PR list changed).
    // When Guard 1 returned 304, the repo is in prListUnchangedRepos — no new PRs exist.
    for (const session of sessions) {
      if (!session.branch) continue;
      if (
        session.metadata["prAutoDetect"] === "off" ||
        session.metadata["prAutoDetect"] === "false"
      )
        continue;
      if (session.metadata["role"] === "orchestrator" || session.id.endsWith("-orchestrator"))
        continue;
      // Skip detectPR only if we already have a PR on the configured project repo.
      // This allows detecting additional PRs on different repos (multi-repo support).
      const sessionPRs = normalizeSessionPRs(session);
      const trackedRepos = new Set(sessionPRs.map((p) => `${p.owner}/${p.repo}`));
      const projectRepoForDetect = config.projects[session.projectId]?.repo;
      // primaryPR.branch is always the session branch (metadata doesn't store per-PR branches),
      // so use the lifecycle closed-state alone to allow re-detection after a PR is rejected.
      const primaryPRIsClosed = session.lifecycle.pr.state === "closed";
      if (
        sessionPRs.length > 0 &&
        projectRepoForDetect &&
        trackedRepos.has(projectRepoForDetect) &&
        !primaryPRIsClosed
      ) {
        continue;
      }

      const project = config.projects[session.projectId];
      if (!project?.repo || !project.scm?.plugin) continue;

      // Skip if Guard 1 confirmed no PR list changes for this repo
      if (prListUnchangedRepos.has(project.repo)) continue;

      const scm = registry.get<SCM>("scm", project.scm.plugin);
      if (!scm?.detectPR) continue;

      try {
        const detectedPR = await scm.detectPR(session, project);
        if (detectedPR) {
          // Track by owner/repo/number — allows multiple PRs on the same repo
          // in the same session (e.g. agent opens PR #10 and PR #11 both on acme/main-app).
          // Only skip if we already have this exact PR number on this exact repo.
          // If the existing PR on the same repo is closed, replace it with the new one.
          const alreadyTracked = sessionPRs.some(
            (p) =>
              p.owner === detectedPR.owner &&
              p.repo === detectedPR.repo &&
              p.number === detectedPR.number
          );
          if (!alreadyTracked) {
            // Remove any closed PRs on the same repo before adding the new one.
            // Open PRs on the same repo are kept — multiple open PRs per repo are valid.
            session.prs = session.prs
              .filter(
                (p) =>
                  !(
                    p.owner === detectedPR.owner &&
                    p.repo === detectedPR.repo &&
                    p.number !== detectedPR.number &&
                    prEnrichmentCache.get(`${p.owner}/${p.repo}#${p.number}`)?.state === "closed"
                  )
              )
              .concat(detectedPR);
          }
          session.prs = dedupePrInfos(session.prs);
          // pr is always the primary (first) PR
          session.pr = session.prs[0] ?? detectedPR;
          const sessionsDir = getProjectSessionsDir(session.projectId);
          const allPrUrls = [...new Set(session.prs.map((p) => p.url))].join(",");
          updateMetadata(sessionsDir, session.id, {
            pr: session.pr.url,
            prs: allPrUrls,
          });
          recordActivityEvent({
            projectId: session.projectId,
            sessionId: session.id,
            source: "scm",
            kind: "scm.detect_pr_succeeded",
            summary: `PR #${detectedPR.number} detected`,
            data: {
              plugin: project.scm.plugin,
              prNumber: detectedPR.number,
              prUrl: detectedPR.url,
              prOwner: detectedPR.owner,
              prRepo: detectedPR.repo,
            },
          });
        }
      } catch (error) {
        const errorMsg = error instanceof Error ? error.message : String(error);
        observer?.recordOperation?.({
          metric: "lifecycle_poll",
          operation: "scm.detect_pr",
          outcome: "failure",
          correlationId: createCorrelationId("detect-pr"),
          projectId: session.projectId,
          sessionId: session.id,
          reason: errorMsg,
          level: "warn",
        });
        recordActivityEvent({
          projectId: session.projectId,
          sessionId: session.id,
          source: "scm",
          kind: "scm.detect_pr_failed",
          level: "warn",
          summary: `detect_pr failed for ${session.id}`,
          data: {
            plugin: project.scm.plugin,
            errorMessage: errorMsg,
          },
        });
      }
    }
  }

  /**
   * Persist batch enrichment data to session metadata files.
   * The web dashboard reads this instead of calling GitHub API.
   */
  function persistPREnrichmentToMetadata(sessions: Session[]): void {
    for (const session of sessions) {
      const sessionPRs = normalizeSessionPRs(session);
      if (!session.pr) continue;
      const project = config.projects[session.projectId];
      if (!project) continue;
      const sessionsDir = getProjectSessionsDir(session.projectId);
      const cleanupUpdates = indexedPRMetadataCleanup(session, sessionPRs.length);
      if (Object.keys(cleanupUpdates).length > 0) {
        updateMetadata(sessionsDir, session.id, cleanupUpdates);
        session.metadata = Object.fromEntries(
          Object.entries(session.metadata).filter(([key]) => cleanupUpdates[key] === undefined),
        );
      }

      const prKey = `${session.pr.owner}/${session.pr.repo}#${session.pr.number}`;
      const cached = prEnrichmentCache.get(prKey);
      if (cached) {
        const blob = JSON.stringify({
          state: cached.state,
          ciStatus: cached.ciStatus,
          reviewDecision: cached.reviewDecision,
          mergeable: cached.mergeable,
          title: cached.title,
          additions: cached.additions,
          deletions: cached.deletions,
          isDraft: cached.isDraft,
          hasConflicts: cached.hasConflicts,
          isBehind: cached.isBehind,
          blockers: cached.blockers,
          ciChecks: cached.ciChecks?.map((c) => ({
            name: c.name,
            status: c.status,
            url: c.url,
          })),
          enrichedAt: new Date().toISOString(),
        });
        if (session.metadata["prEnrichment"] !== blob) {
          updateMetadata(sessionsDir, session.id, { prEnrichment: blob });
          session.metadata["prEnrichment"] = blob;
        }
        // Keep in-memory isDraft in sync with enrichment data
        if (cached.isDraft !== undefined && session.pr) {
          session.pr.isDraft = cached.isDraft;
        }
      }

      for (let i = 1; i < sessionPRs.length; i++) {
        const secondaryPR = sessionPRs[i];
        if (!secondaryPR) continue;
        const secondaryKey = `${secondaryPR.owner}/${secondaryPR.repo}#${secondaryPR.number}`;
        const secondaryCached = prEnrichmentCache.get(secondaryKey);
        if (!secondaryCached) continue;
        const secondaryBlob = JSON.stringify({
          state: secondaryCached.state,
          ciStatus: secondaryCached.ciStatus,
          reviewDecision: secondaryCached.reviewDecision,
          mergeable: secondaryCached.mergeable,
          title: secondaryCached.title,
          additions: secondaryCached.additions,
          deletions: secondaryCached.deletions,
          isDraft: secondaryCached.isDraft,
          hasConflicts: secondaryCached.hasConflicts,
          isBehind: secondaryCached.isBehind,
          blockers: secondaryCached.blockers,
          ciChecks: secondaryCached.ciChecks?.map((c) => ({
            name: c.name,
            status: c.status,
            url: c.url,
          })),
          enrichedAt: new Date().toISOString(),
        });
        const metaKey = `prEnrichment_${i}`;
        if (session.metadata[metaKey] !== secondaryBlob) {
          updateMetadata(sessionsDir, session.id, { [metaKey]: secondaryBlob });
          session.metadata[metaKey] = secondaryBlob;
        }
        // Keep in-memory isDraft in sync with enrichment data
        if (secondaryCached.isDraft !== undefined) {
          secondaryPR.isDraft = secondaryCached.isDraft;
        }
      }
    }
  }

  /** Check if idle time exceeds the agent-stuck threshold. */
  function isIdleBeyondThreshold(session: Session, idleTimestamp: Date): boolean {
    const stuckReaction = getReactionConfigForSession(session, "agent-stuck");
    const thresholdStr = stuckReaction?.threshold;
    if (typeof thresholdStr !== "string") return false;
    const stuckThresholdMs = parseDuration(thresholdStr);
    if (stuckThresholdMs <= 0) return false;
    const idleMs = Date.now() - idleTimestamp.getTime();
    return idleMs > stuckThresholdMs;
  }

  function isBranchOwnedByAnotherActiveWorker(
    session: Session,
    branch: string,
    siblingSessions: Session[],
    allSessionPrefixes: string[],
  ): boolean {
    return siblingSessions.some((other) => {
      if (other.id === session.id) return false;
      if (other.projectId !== session.projectId) return false;
      if (TERMINAL_STATUSES.has(other.status)) return false;

      const otherProject = config.projects[other.projectId];
      if (!otherProject) return false;

      const otherRole = resolveSessionRole(
        other.id,
        other.metadata,
        otherProject.sessionPrefix,
        allSessionPrefixes,
      );
      return otherRole === "worker" && other.branch === branch;
    });
  }

  function acquireBranchAdoptionReservation(session: Session, branch: string): string | null {
    const reservationKey = `${session.projectId}:${branch}`;
    const existingOwner = branchAdoptionReservations.get(reservationKey);
    if (existingOwner && existingOwner !== session.id) {
      return null;
    }
    branchAdoptionReservations.set(reservationKey, session.id);
    return reservationKey;
  }

  function releaseBranchAdoptionReservation(reservationKey: string, sessionId: SessionId): void {
    if (branchAdoptionReservations.get(reservationKey) === sessionId) {
      branchAdoptionReservations.delete(reservationKey);
    }
  }

  async function refreshTrackedBranch(
    session: Session,
    siblingSessions?: Session[],
  ): Promise<void> {
    const project = config.projects[session.projectId];
    if (!project) return;

    const allSessionPrefixes = Object.values(config.projects).map((p) => p.sessionPrefix);
    const sessionRole = resolveSessionRole(
      session.id,
      session.metadata,
      project.sessionPrefix,
      allSessionPrefixes,
    );
    const workspacePath = session.workspacePath;
    const canRefreshTrackedBranch =
      sessionRole === "worker" &&
      workspacePath !== null &&
      (!session.pr || session.lifecycle.pr.state === "closed");

    if (!canRefreshTrackedBranch) return;

    const branchProbe = await readWorkspaceBranch(workspacePath);
    if (branchProbe.kind === "detached") {
      if (session.branch !== null) {
        session.branch = null;
        updateSessionMetadata(session, { branch: "" });
      }
      return;
    }

    if (branchProbe.kind !== "branch" || branchProbe.branch === session.branch) {
      return;
    }

    const reservationKey = acquireBranchAdoptionReservation(session, branchProbe.branch);
    if (!reservationKey) return;

    try {
      const sessionsForConflictCheck =
        siblingSessions ?? (await sessionManager.list(session.projectId));
      if (
        !isBranchOwnedByAnotherActiveWorker(
          session,
          branchProbe.branch,
          sessionsForConflictCheck,
          allSessionPrefixes,
        )
      ) {
        session.branch = branchProbe.branch;
        updateSessionMetadata(session, { branch: branchProbe.branch });
      }
    } finally {
      releaseBranchAdoptionReservation(reservationKey, session.id);
    }
  }

  /** Determine current status for a session by polling plugins. */
  async function determineStatus(session: Session): Promise<DeterminedStatus> {
    const project = config.projects[session.projectId];
    if (!project) {
      return {
        status: session.status,
        evidence: "project_missing",
        detectingAttempts: parseAttemptCount(session.metadata["detectingAttempts"]),
      };
    }

    // Sessions held by an unresolved dependency stay in the blocked pre-state
    // until the scheduler clears their prerequisites. Skip all probing and
    // status inference so the polling loop never promotes them to "working"
    // (the SPAWNING → WORKING fallthrough below would otherwise un-block them).
    if (isBlockedByDependency(session.lifecycle)) {
      return {
        status: session.status,
        evidence: "blocked_by_dependency",
        detectingAttempts: parseAttemptCount(session.metadata["detectingAttempts"]),
        skipMetadataWrite: true,
      };
    }

    const lifecycle = cloneLifecycle(session.lifecycle);
    const nowIso = new Date().toISOString();
    const agentName = session.metadata["agent"];
    const agent = agentName ? registry.get<Agent>("agent", agentName) : null;
    const scm = project.scm?.plugin ? registry.get<SCM>("scm", project.scm.plugin) : null;
    let detectedIdleTimestamp: Date | null = null;
    let idleWasBlocked = false;
    const canProbeRuntimeIdentity = session.status !== SESSION_STATUS.SPAWNING;
    const currentDetectingAttempts = parseAttemptCount(session.metadata["detectingAttempts"]);
    const currentDetectingStartedAt = session.metadata["detectingStartedAt"] || undefined;
    const currentDetectingEvidenceHash = session.metadata["detectingEvidenceHash"] || undefined;

    const commit = (
      decision: LifecycleDecision = {
        status: deriveLegacyStatus(lifecycle),
        evidence: "lifecycle_commit",
        detecting: { attempts: currentDetectingAttempts },
      },
    ): DeterminedStatus => {
      commitLifecycleDecisionInPlace(lifecycle, decision, nowIso);
      session.lifecycle = lifecycle;
      session.status = decision.status;
      session.activitySignal = activitySignal;
      return {
        status: decision.status,
        evidence: decision.evidence,
        detectingAttempts: decision.detecting.attempts,
        detectingStartedAt: decision.detecting.startedAt,
        detectingEvidenceHash: decision.detecting.evidenceHash,
      };
    };

    let runtimeProbe: ProbeResult = { state: "unknown", failed: false };
    if (session.runtimeHandle && canProbeRuntimeIdentity) {
      const runtime = registry.get<Runtime>("runtime", project.runtime ?? config.defaults.runtime);
      if (runtime) {
        try {
          const alive = await runtime.isAlive(session.runtimeHandle);
          lifecycle.runtime.lastObservedAt = nowIso;
          runtimeProbe = { state: alive ? "alive" : "dead", failed: false };
          if (alive) {
            lifecycle.runtime.state = "alive";
            lifecycle.runtime.reason = "process_running";
          } else {
            lifecycle.runtime.state = "missing";
            lifecycle.runtime.reason =
              session.runtimeHandle.runtimeName === "tmux" ? "tmux_missing" : "process_missing";
          }
        } catch (err) {
          lifecycle.runtime.state = "probe_failed";
          lifecycle.runtime.reason = "probe_error";
          lifecycle.runtime.lastObservedAt = nowIso;
          runtimeProbe = { state: "unknown", failed: true };
          recordActivityEvent({
            projectId: session.projectId,
            sessionId: session.id,
            source: "runtime",
            kind: "runtime.probe_failed",
            level: "warn",
            summary: `runtime.isAlive probe failed for ${session.id}`,
            data: {
              runtimeName: session.runtimeHandle.runtimeName,
              errorMessage: err instanceof Error ? err.message : String(err),
            },
          });
        }
      }
    }

    let activitySignal = createActivitySignal("unavailable");
    let processProbe: ProbeResult = { state: "unknown", failed: false };
    let activityEvidence = formatActivitySignalEvidence(activitySignal);

    if (agent && (session.runtimeHandle || session.workspacePath)) {
      try {
        if (
          agent.recordActivity &&
          session.workspacePath &&
          session.runtimeHandle &&
          canProbeRuntimeIdentity
        ) {
          try {
            const runtime = registry.get<Runtime>(
              "runtime",
              project.runtime ?? config.defaults.runtime,
            );
            const terminalOutput = runtime
              ? await runtime.getOutput(session.runtimeHandle, 10)
              : "";
            if (terminalOutput) {
              await agent.recordActivity(session, terminalOutput);
            }
          } catch (error) {
            observer?.recordOperation?.({
              metric: "lifecycle_poll",
              operation: "activity.record",
              outcome: "failure",
              correlationId: createCorrelationId("lifecycle-poll"),
              projectId: session.projectId,
              sessionId: session.id,
              reason: error instanceof Error ? error.message : String(error),
              level: "warn",
            });
          }
        }

        // Prefer this project's idle/ready threshold (preserves a carried
        // startup-only project's own threshold) over the top-level default.
        const readyThresholdMs =
          config.projects[session.projectId]?.readyThresholdMs ?? config.readyThresholdMs;
        const detectedActivity = await agent.getActivityState(session, readyThresholdMs);
        if (detectedActivity) {
          activitySignal = classifyActivitySignal(detectedActivity, "native");
          activityEvidence = formatActivitySignalEvidence(activitySignal);
          lifecycle.runtime.lastObservedAt = nowIso;
          const prevActivity = activityStateCache.get(session.id);
          activityStateCache.set(session.id, detectedActivity.state);
          if (prevActivity !== undefined && prevActivity !== detectedActivity.state) {
            recordActivityEvent({
              projectId: session.projectId,
              sessionId: session.id,
              source: "lifecycle",
              kind: "activity.transition",
              summary: `${prevActivity} → ${detectedActivity.state}`,
              data: { from: prevActivity, to: detectedActivity.state },
            });
          }
          if (lifecycle.runtime.state !== "missing" && lifecycle.runtime.state !== "probe_failed") {
            lifecycle.runtime.state = "alive";
            lifecycle.runtime.reason = "process_running";
          }
          if (detectedActivity.state === "waiting_input") {
            return commit({
              status: SESSION_STATUS.NEEDS_INPUT,
              evidence: activityEvidence,
              detecting: { attempts: 0 },
              sessionState: "needs_input",
              sessionReason: "awaiting_user_input",
            });
          }
          if (detectedActivity.state === "exited" && canProbeRuntimeIdentity) {
            processProbe = { state: "dead", failed: false };
            lifecycle.runtime.state = "exited";
            lifecycle.runtime.reason = "process_missing";
          }

          if (hasPositiveIdleEvidence(activitySignal)) {
            detectedIdleTimestamp = activitySignal.timestamp;
            idleWasBlocked = activitySignal.activity === "blocked";
          }
        } else if (session.runtimeHandle && canProbeRuntimeIdentity) {
          activitySignal = createActivitySignal("null", { source: "native" });
          activityEvidence = formatActivitySignalEvidence(activitySignal);
          const runtime = registry.get<Runtime>(
            "runtime",
            project.runtime ?? config.defaults.runtime,
          );
          const terminalOutput = runtime ? await runtime.getOutput(session.runtimeHandle, 10) : "";
          if (terminalOutput) {
            const activity = agent.detectActivity(terminalOutput);
            activitySignal = classifyActivitySignal({ state: activity }, "terminal");
            activityEvidence = formatActivitySignalEvidence(activitySignal);
            if (activity === "waiting_input") {
              return commit({
                status: SESSION_STATUS.NEEDS_INPUT,
                evidence: activityEvidence,
                detecting: { attempts: 0 },
                sessionState: "needs_input",
                sessionReason: "awaiting_user_input",
              });
            }

            try {
              const processAlive = await agent.isProcessRunning(session.runtimeHandle);
              processProbe = processProbeResultToProbeResult(processAlive);
              if (processAlive === false) {
                lifecycle.runtime.state = "exited";
                lifecycle.runtime.reason = "process_missing";
                lifecycle.runtime.lastObservedAt = nowIso;
              }
            } catch (err) {
              processProbe = { state: "unknown", failed: true };
              recordActivityEvent({
                projectId: session.projectId,
                sessionId: session.id,
                source: "agent",
                kind: "agent.process_probe_failed",
                level: "warn",
                summary: `agent.isProcessRunning failed for ${session.id}`,
                data: {
                  agentName,
                  where: "fallback",
                  errorMessage: err instanceof Error ? err.message : String(err),
                },
              });
            }
          }
        } else {
          activitySignal = createActivitySignal("null", { source: "native" });
          activityEvidence = formatActivitySignalEvidence(activitySignal);
        }
      } catch (err) {
        activitySignal = createActivitySignal("probe_failure", { source: "native" });
        activityEvidence = formatActivitySignalEvidence(activitySignal);
        recordActivityEvent({
          projectId: session.projectId,
          sessionId: session.id,
          source: "agent",
          kind: "agent.activity_probe_failed",
          level: "warn",
          summary: `activity probing failed for ${session.id}`,
          data: {
            agentName,
            errorMessage: err instanceof Error ? err.message : String(err),
          },
        });
        if (
          lifecycle.session.state === "stuck" ||
          lifecycle.session.state === "needs_input" ||
          lifecycle.session.state === "detecting"
        ) {
          return commit({
            status: session.status,
            evidence: activityEvidence,
            detecting: { attempts: currentDetectingAttempts },
          });
        }
        return commit(
          createDetectingDecision({
            currentAttempts: currentDetectingAttempts,
            idleWasBlocked,
            evidence: activityEvidence,
            detectingStartedAt: currentDetectingStartedAt,
            previousEvidenceHash: currentDetectingEvidenceHash,
          }),
        );
      }
    }

    if (
      processProbe.state === "unknown" &&
      !processProbe.indeterminate &&
      session.runtimeHandle &&
      canProbeRuntimeIdentity &&
      agent
    ) {
      try {
        const processAlive = await agent.isProcessRunning(session.runtimeHandle);
        processProbe = processProbeResultToProbeResult(processAlive);
        if (processAlive === false) {
          lifecycle.runtime.state = "exited";
          lifecycle.runtime.reason = "process_missing";
          lifecycle.runtime.lastObservedAt = nowIso;
        }
      } catch (err) {
        processProbe = { state: "unknown", failed: true };
        recordActivityEvent({
          projectId: session.projectId,
          sessionId: session.id,
          source: "agent",
          kind: "agent.process_probe_failed",
          level: "warn",
          summary: `agent.isProcessRunning failed for ${session.id}`,
          data: {
            agentName,
            where: "standalone",
            errorMessage: err instanceof Error ? err.message : String(err),
          },
        });
      }
    }

    if (processProbe.indeterminate) {
      recordActivityEvent({
        projectId: session.projectId,
        sessionId: session.id,
        source: "agent",
        kind: "agent.process_probe_failed",
        level: "warn",
        summary: `agent.isProcessRunning indeterminate for ${session.id}`,
        data: {
          agentName,
          reason: "probe_indeterminate",
        },
      });
      return {
        status: session.status,
        evidence: session.metadata["lifecycleEvidence"] ?? "process_probe_indeterminate",
        detectingAttempts: currentDetectingAttempts,
        detectingStartedAt: currentDetectingStartedAt,
        detectingEvidenceHash: currentDetectingEvidenceHash,
        skipMetadataWrite: true,
      };
    }

    const probeDecision = resolveProbeDecision({
      currentAttempts: currentDetectingAttempts,
      runtimeProbe,
      processProbe,
      canProbeRuntimeIdentity,
      activitySignal,
      activityEvidence,
      idleWasBlocked,
      detectingStartedAt: currentDetectingStartedAt,
      previousEvidenceHash: currentDetectingEvidenceHash,
    });
    if (probeDecision) {
      return commit(probeDecision);
    }

    // detectPR is handled in populatePREnrichmentCache (gated by Guard 1 ETag).
    // By this point, session.pr is already set if a PR was discovered.

    if (session.pr && scm) {
      // Two explicit loop-control escalations park the session in needs_input
      // while its PR stays open, overriding SCM-derived status:
      //  • reviewRoundsEscalated — the bot review→fix loop exceeded its round cap.
      //  • stuckNudgeEscalated   — a stuck/idle agent exhausted its auto-nudge
      //    budget for unaddressed review comments (see maybeNudgeStuckAgent).
      // Without this, SCM enrichment below — which sees bot comments only as
      // generic threads, not a "changes requested" review — would re-derive
      // pr_open/mergeable/stuck and bury the escalation. Both latches are cleared
      // by maybeDispatchReviewBacklog once the underlying comments clear. This is
      // the one place SCM ground truth yields to an explicit loop-control decision.
      //
      // Terminal PR states still win: a human may resolve the loop by merging or
      // closing the PR directly while threads are unresolved. In that case
      // fall through so the enrichment/live-API path returns merged/closed —
      // which also drops the pr.state to non-open, letting maybeDispatchReviewBacklog
      // clear the latch and unblock merge cleanup / close notifications.
      const reviewRoundsEscalated = session.metadata["reviewRoundsEscalated"] === "true";
      const stuckNudgeEscalated = session.metadata["stuckNudgeEscalated"] === "true";
      if (reviewRoundsEscalated || stuckNudgeEscalated) {
        const escalatedPRKey = `${session.pr.owner}/${session.pr.repo}#${session.pr.number}`;
        const escalatedEnrichment = prEnrichmentCache.get(escalatedPRKey);
        let prIsTerminal =
          escalatedEnrichment?.state === "merged" || escalatedEnrichment?.state === "closed";
        // Cache miss (batch enrichment unimplemented or the fetch failed): consult
        // live PR state so a human merge/close still releases the latch — otherwise
        // needs_input would stick and merge cleanup / close notifications never run.
        if (!escalatedEnrichment && scm.getPRState) {
          try {
            const liveState = await scm.getPRState(session.pr);
            prIsTerminal = liveState === "merged" || liveState === "closed";
          } catch {
            // Couldn't confirm — keep holding the latch until the next poll.
          }
        }
        if (!prIsTerminal) {
          if (lifecycle.pr.state === "none") lifecycle.pr.state = "open";
          if (lifecycle.pr.reason === "not_created") lifecycle.pr.reason = "in_progress";
          lifecycle.pr.number = session.pr.number;
          lifecycle.pr.url = session.pr.url;
          lifecycle.pr.lastObservedAt = nowIso;
          return commit({
            status: SESSION_STATUS.NEEDS_INPUT,
            evidence: reviewRoundsEscalated ? "review_rounds_escalated" : "stuck_nudge_escalated",
            detecting: { attempts: 0 },
            sessionState: "needs_input",
            sessionReason: "awaiting_user_input",
          });
        }
      }
      try {
        const prKey = `${session.pr.owner}/${session.pr.repo}#${session.pr.number}`;
        const cachedData = prEnrichmentCache.get(prKey);
        if (lifecycle.pr.state === "none") {
          lifecycle.pr.state = "open";
        }
        if (lifecycle.pr.reason === "not_created") {
          lifecycle.pr.reason = "in_progress";
        }
        lifecycle.pr.number = session.pr.number;
        lifecycle.pr.url = session.pr.url;
        lifecycle.pr.lastObservedAt = nowIso;
        const shouldEscalateIdleToStuck =
          detectedIdleTimestamp !== null && hasPositiveIdleEvidence(activitySignal)
            ? isIdleBeyondThreshold(session, detectedIdleTimestamp)
            : false;

        if (cachedData) {
          // When session has multiple PRs, aggregate enrichment across all of them.
          // ci_failed if ANY fails; approved/merged only when ALL pass.
          if (session.prs.length > 1) {
            const allEnrichments = session.prs
              .map((p) => prEnrichmentCache.get(`${p.owner}/${p.repo}#${p.number}`))
              .filter((e): e is PREnrichmentData => e !== undefined);

            if (allEnrichments.length === session.prs.length) {
              const aggregated: PREnrichmentData = {
                ciStatus: allEnrichments.some((e) => e.ciStatus === "failing")
                  ? "failing"
                  : allEnrichments.every((e) => e.ciStatus === "passing" || e.ciStatus === "none")
                    ? "passing"
                    : "pending",
                reviewDecision: allEnrichments.some(
                  (e) => e.reviewDecision === "changes_requested",
                )
                  ? "changes_requested"
                  : allEnrichments.every((e) => e.reviewDecision === "approved")
                    ? "approved"
                    : allEnrichments.every((e) => e.reviewDecision === "none")
                      ? "none"
                      : "pending",
                state: allEnrichments.every((e) => e.state === "merged")
                  ? "merged"
                  : allEnrichments.some((e) => e.state === "open")
                    ? "open"
                    : "closed",
                mergeable: allEnrichments.every((e) => e.mergeable),
                blockers: [...new Set(allEnrichments.flatMap((e) => e.blockers ?? []))],
                title: cachedData.title,
                additions: cachedData.additions,
                deletions: cachedData.deletions,
                isDraft: allEnrichments.some((e) => e.isDraft),
                hasConflicts: allEnrichments.some((e) => e.hasConflicts),
                isBehind: allEnrichments.some((e) => e.isBehind),
              };
              return commit(
                resolvePREnrichmentDecision(aggregated, {
                  shouldEscalateIdleToStuck,
                  idleWasBlocked,
                  activityEvidence,
                }),
              );
            }
          }
          // Partial cache miss for multi-PR session: never decide on primary PR
          // alone — fall through to the live-API check that verifies all PRs.
          if (session.prs.length <= 1) {
            return commit(
              resolvePREnrichmentDecision(cachedData, {
                shouldEscalateIdleToStuck,
                idleWasBlocked,
                activityEvidence,
              }),
            );
          }
          // intentional fall-through to live-API block below
        }

        // Batch enrichment cache miss — fall back to getPRState for terminal
        // states (merged/closed) only. Detecting these promptly prevents
        // delayed cleanup. Non-terminal state updates wait for the next batch
        // cycle (30s) to avoid ~110 individual REST calls per 15-min window.
        try {
          if (session.prs.length > 1) {
            // Multi-PR: only terminate when ALL PRs are in a terminal state.
            const states = await Promise.all(session.prs.map((p) => scm.getPRState(p)));
            if (states.every((s) => s === "merged" || s === "closed")) {
              const prState = states.every((s) => s === "merged") ? "merged" : "closed";
              return commit(
                resolvePRLiveDecision({
                  prState,
                  ciStatus: "none",
                  reviewDecision: "none",
                  mergeable: false,
                  shouldEscalateIdleToStuck,
                  idleWasBlocked,
                  activityEvidence,
                }),
              );
            }
          } else {
            const prState = await scm.getPRState(session.pr);
            if (prState === "merged" || prState === "closed") {
              return commit(
                resolvePRLiveDecision({
                  prState,
                  ciStatus: "none",
                  reviewDecision: "none",
                  mergeable: false,
                  shouldEscalateIdleToStuck,
                  idleWasBlocked,
                  activityEvidence,
                }),
              );
            }
          }
        } catch (err) {
          // Best-effort — batch will retry next cycle. Record AE evidence so
          // RCA can answer "why didn't AO transition to merged/closed in time?"
          recordActivityEvent({
            projectId: session.projectId,
            sessionId: session.id,
            source: "scm",
            kind: "scm.poll_pr_failed",
            level: "warn",
            summary: `getPRState failed for PR #${session.pr.number}`,
            data: {
              plugin: project.scm?.plugin,
              prNumber: session.pr.number,
              prUrl: session.pr.url,
              errorMessage: err instanceof Error ? err.message : String(err),
            },
          });
        }
      } catch (error) {
        observer?.recordOperation?.({
          metric: "lifecycle_poll",
          operation: "scm.poll_pr",
          outcome: "failure",
          correlationId: createCorrelationId("lifecycle-poll"),
          projectId: session.projectId,
          sessionId: session.id,
          reason: error instanceof Error ? error.message : String(error),
          level: "warn",
        });
      }
    }

    // Fresh agent reports outrank weak inference (idle-beyond-threshold /
    // default-to-working) but runtime death, activity waiting_input, and SCM
    // ground truth already short-circuited above. Orchestrator sessions and
    // terminal states are skipped intentionally — `lifecycle.session.kind` is
    // the authoritative source (string-matching role/id suffixes misses
    // numbered orchestrator IDs like `${prefix}-orchestrator-1`).
    const agentReport = readAgentReport(session.metadata);
    if (
      agentReport &&
      isAgentReportFresh(agentReport) &&
      lifecycle.session.kind !== "orchestrator" &&
      lifecycle.session.state !== "terminated" &&
      lifecycle.session.state !== "done"
    ) {
      const mapped = mapAgentReportToLifecycle(agentReport.state);
      return commit({
        status: deriveLegacyStatus({
          ...lifecycle,
          session: {
            ...lifecycle.session,
            state: mapped.sessionState,
            reason: mapped.sessionReason,
          },
        }),
        evidence: `agent_report:${agentReport.state}`,
        detecting: { attempts: 0 },
        sessionState: mapped.sessionState,
        sessionReason: mapped.sessionReason,
      });
    }

    if (
      detectedIdleTimestamp &&
      hasPositiveIdleEvidence(activitySignal) &&
      isIdleBeyondThreshold(session, detectedIdleTimestamp)
    ) {
      return commit({
        status: SESSION_STATUS.STUCK,
        evidence: `idle_beyond_threshold ${activityEvidence}`,
        detecting: { attempts: 0 },
        sessionState: "stuck",
        sessionReason: idleWasBlocked ? "error_in_process" : "probe_failure",
      });
    }

    if (
      isWeakActivityEvidence(activitySignal) &&
      (session.status === SESSION_STATUS.DETECTING ||
        session.status === SESSION_STATUS.STUCK ||
        session.status === SESSION_STATUS.NEEDS_INPUT ||
        lifecycle.session.state === "detecting" ||
        lifecycle.session.state === "stuck" ||
        lifecycle.session.state === "needs_input")
    ) {
      const preservingProbeFailureStuck =
        activitySignal.state === "unavailable" &&
        lifecycle.session.state === "stuck" &&
        lifecycle.session.reason === "probe_failure" &&
        runtimeProbe.state === "alive" &&
        !runtimeProbe.failed;

      if (preservingProbeFailureStuck) {
        return commit({
          status: SESSION_STATUS.DETECTING,
          evidence: activityEvidence,
          detecting: { attempts: 0 },
          sessionState: "detecting",
          sessionReason: "probe_failure",
        });
      }

      return commit({
        status: deriveLegacyStatus(lifecycle),
        evidence: activityEvidence,
        detecting: { attempts: 0 },
      });
    }

    if (
      session.status === SESSION_STATUS.SPAWNING ||
      session.status === SESSION_STATUS.DETECTING ||
      session.status === SESSION_STATUS.STUCK ||
      session.status === SESSION_STATUS.NEEDS_INPUT
    ) {
      return commit({
        status: SESSION_STATUS.WORKING,
        evidence: activityEvidence,
        detecting: { attempts: 0 },
        sessionState: "working",
        sessionReason: "task_in_progress",
      });
    }

    return commit({
      status: session.status,
      evidence: activityEvidence,
      detecting: { attempts: 0 },
    });
  }

  /**
   * Gather cheap, already-available risk signals for confidence scoring (#12).
   * Reads review-round churn from metadata, CI-failure count from the ci-failed
   * reaction tracker, diff size from cached PR enrichment, and the riskiest open
   * automated review finding from the code-review store. Never throws — missing
   * data simply contributes no penalty.
   */
  function gatherConfidenceSignals(session: Session): ConfidenceSignals {
    // Use the CUMULATIVE round count: the review backlog loop clears
    // reviewRoundCount as soon as the bot-review loop is satisfied, so a PR that
    // churned through several rounds then went review-clean (while CI was still
    // pending) would otherwise score as zero churn for a later approved-and-green
    // check. reviewRoundCountTotal survives satisfaction and is only reset when
    // the PR reaches a terminal state (#12 review). Fall back to the live count.
    const reviewRounds = Math.max(
      parseAttemptCount(session.metadata["reviewRoundCountTotal"]),
      parseAttemptCount(session.metadata["reviewRoundCount"]),
    );
    const ciTracker = reactionTrackers.get(`${session.id}:ci-failed`);
    const ciFailureCount = ciTracker?.attempts ?? 0;

    // Sum the diff across ALL of the session's PRs (a session can own several),
    // so a large secondary PR still contributes its risk — mirroring how the
    // lifecycle code aggregates status over session.prs (#12 review). Deduped
    // without mutating the session (unlike normalizeSessionPRs).
    const sessionPRs = dedupePrInfos(
      session.prs.length > 0 ? session.prs : session.pr ? [session.pr] : [],
    );
    let diffSize: number | undefined;
    for (const pr of sessionPRs) {
      const prEnrichment = prEnrichmentCache.get(`${pr.owner}/${pr.repo}#${pr.number}`);
      if (
        prEnrichment &&
        (prEnrichment.additions !== undefined || prEnrichment.deletions !== undefined)
      ) {
        diffSize = (diffSize ?? 0) + (prEnrichment.additions ?? 0) + (prEnrichment.deletions ?? 0);
      }
    }

    let maxFindingSeverity: CodeReviewSeverity | null = null;
    let maxFindingConfidence: number | null = null;
    try {
      const store = createCodeReviewStore(session.projectId);
      // Findings from a superseded (outdated) run describe a stale target SHA the
      // agent has since pushed past — they must not keep holding autonomous
      // actions, so drop them entirely.
      const outdatedRunIds = new Set(
        store.listRuns({ linkedSessionId: session.id, status: "outdated" }).map((run) => run.id),
      );
      // An "unresolved" finding is one still awaiting a fix: freshly surfaced
      // (`open`) OR already delivered to the agent but not yet resolved
      // (`sent_to_agent`). Both represent outstanding review risk.
      const unresolvedFindings = store
        .listFindings({ linkedSessionId: session.id })
        .filter(
          (finding) =>
            (finding.status === "open" || finding.status === "sent_to_agent") &&
            !outdatedRunIds.has(finding.runId),
        );
      for (const finding of unresolvedFindings) {
        const currentRank = maxFindingSeverity ? CODE_REVIEW_SEVERITY_RANK[maxFindingSeverity] : 0;
        const findingRank = CODE_REVIEW_SEVERITY_RANK[finding.severity];
        // Tie-break on confidence, treating a MISSING confidence as full (1) —
        // matching how computeConfidence weights it. Using 0 here would let a
        // low-confidence finding wrongly displace an unknown-confidence one of the
        // same severity and shrink the penalty.
        const isRiskier =
          findingRank > currentRank ||
          (findingRank === currentRank &&
            (finding.confidence ?? 1) > (maxFindingConfidence ?? 1));
        if (isRiskier) {
          maxFindingSeverity = finding.severity;
          maxFindingConfidence = finding.confidence ?? null;
        }
      }
    } catch {
      // No code-review store / findings for this project yet — treat as none.
    }

    return { reviewRounds, ciFailureCount, diffSize, maxFindingSeverity, maxFindingConfidence };
  }

  /**
   * Confidence gate (#12): when a reaction opts into confidence scoring
   * (`confidenceThreshold`) and the session scores below it for an autonomous
   * action, notify a human with a question and return `true` (the action must be
   * held). Returns `false` when the action may proceed. Shared by executeReaction
   * and the direct CI-detail dispatch so every autonomous-send path honors it.
   *
   * Only `auto-merge` and `send-to-agent` (auto-fix) are gated. `notify` already
   * defers to a human. `spawn-session` is intentionally NOT gated: the dependency
   * scheduler launches held dependents unconditionally each poll, independent of
   * this reaction, so holding the reaction here can't actually stop the spawn —
   * gating it would only emit a misleading "held" notification while the spawn
   * still happens.
   */
  async function holdForLowConfidence(
    session: Session,
    reactionKey: string,
    action: string,
    reactionConfig: ReactionConfig,
    attempts: number,
  ): Promise<boolean> {
    const confidenceThreshold = reactionConfig.confidenceThreshold;
    if (
      confidenceThreshold === undefined ||
      (action !== "auto-merge" && action !== "send-to-agent")
    ) {
      return false;
    }
    const assessment = computeConfidence(gatherConfidenceSignals(session));
    if (assessment.score >= confidenceThreshold) return false;

    const question = buildConfidenceEscalationQuestion(
      reactionKey,
      action,
      confidenceThreshold,
      assessment,
    );
    recordActivityEvent({
      projectId: session.projectId,
      sessionId: session.id,
      source: "reaction",
      kind: "reaction.escalated",
      level: "warn",
      summary: `reaction ${reactionKey} held: confidence ${assessment.score.toFixed(2)} < ${confidenceThreshold}`,
      data: {
        reactionKey,
        action,
        cause: "low_confidence",
        confidence: assessment.score,
        confidenceThreshold,
        factors: assessment.factors,
      },
    });
    const context = buildEventContext(session, prEnrichmentCache);
    const event = createEvent("reaction.escalated", {
      sessionId: session.id,
      projectId: session.projectId,
      message: question,
      data: buildReactionEscalationNotificationData({
        eventType: "reaction.escalated",
        sessionId: session.id,
        projectId: session.projectId,
        context,
        reactionKey,
        action: "escalated",
        attempts,
        cause: "low_confidence",
        confidence: assessment.score,
        question,
        enrichment: getPREnrichmentForSession(session),
      }),
    });
    await notifyHuman(event, reactionConfig.priority ?? "urgent");
    return true;
  }

  /** Execute a reaction for a session. */
  async function executeReaction(
    session: Session | ReactionSessionContext,
    reactionKey: string,
    reactionConfig: ReactionConfig,
  ): Promise<ReactionResult> {
    const { id: sessionId, projectId } = session;
    const trackerKey = `${sessionId}:${reactionKey}`;
    let tracker = reactionTrackers.get(trackerKey);

    if (!tracker) {
      tracker = { attempts: 0, firstTriggered: new Date() };
      reactionTrackers.set(trackerKey, tracker);
    }

    // Already escalated — wait for the condition to resolve before resuming.
    if (tracker.escalated) {
      return { reactionType: reactionKey, success: true, action: "escalated", escalated: true };
    }

    // Increment attempts before checking escalation
    tracker.attempts++;

    // Check if we should escalate
    const maxRetries = reactionConfig.retries ?? Infinity;
    const escalateAfter = reactionConfig.escalateAfter;
    let shouldEscalate = false;

    if (tracker.attempts > maxRetries) {
      shouldEscalate = true;
    }

    if (typeof escalateAfter === "string") {
      const durationMs = parseDuration(escalateAfter);
      if (durationMs > 0 && Date.now() - tracker.firstTriggered.getTime() > durationMs) {
        shouldEscalate = true;
      }
    }

    if (typeof escalateAfter === "number" && tracker.attempts > escalateAfter) {
      shouldEscalate = true;
    }

    if (shouldEscalate) {
      // Mirror the trigger checks above so the cause matches the gate that
      // actually fired. Numeric escalateAfter is an attempt-count gate, not a
      // duration; without this distinction it gets misattributed to max_duration.
      const escalationCause: "max_retries" | "max_attempts" | "max_duration" =
        tracker.attempts > maxRetries
          ? "max_retries"
          : typeof escalateAfter === "number" && tracker.attempts > escalateAfter
            ? "max_attempts"
            : "max_duration";
      const durationMs = Date.now() - tracker.firstTriggered.getTime();
      recordActivityEvent({
        projectId,
        sessionId,
        source: "reaction",
        kind: "reaction.escalated",
        level: "warn",
        summary: `reaction ${reactionKey} escalated after ${tracker.attempts} attempts`,
        data: {
          reactionKey,
          attempts: tracker.attempts,
          durationSinceFirstMs: durationMs,
          escalationCause,
        },
      });
      // Escalate to human
      const context = buildEventContext(session, prEnrichmentCache);
      const event = createEvent("reaction.escalated", {
        sessionId,
        projectId,
        message: `Reaction '${reactionKey}' escalated after ${tracker.attempts} attempts`,
        data: buildReactionEscalationNotificationData({
          eventType: "reaction.escalated",
          sessionId,
          projectId,
          context,
          reactionKey,
          action: "escalated",
          attempts: tracker.attempts,
          cause: escalationCause,
          durationMs,
          enrichment: getPREnrichmentForSession(session),
        }),
      });
      await notifyHuman(event, reactionConfig.priority ?? "urgent");

      // Mark as escalated — silences further dispatches until the underlying
      // condition resolves and clearReactionTracker() is called explicitly.
      tracker.escalated = true;

      return {
        reactionType: reactionKey,
        success: true,
        action: "escalated",
        escalated: true,
      };
    }

    // Execute the reaction action
    const action = reactionConfig.action ?? "notify";

    // Confidence gate (#12): hold an opted-in autonomous action below its
    // configured threshold and escalate to a human instead. Synthetic system
    // sessions (no lifecycle) are skipped — they carry no metadata/PR to score.
    if (
      "lifecycle" in session &&
      (await holdForLowConfidence(session, reactionKey, action, reactionConfig, tracker.attempts))
    ) {
      // Latch escalated so the action stays held (mirrors escalateAfter) until
      // the underlying condition resolves and the tracker is cleared.
      tracker.escalated = true;
      return {
        reactionType: reactionKey,
        success: true,
        action: "escalated",
        escalated: true,
      };
    }

    switch (action) {
      case "send-to-agent": {
        if (reactionConfig.message) {
          try {
            await sessionManager.send(sessionId, reactionConfig.message);
            recordActivityEvent({
              projectId,
              sessionId,
              source: "reaction",
              kind: "reaction.action_succeeded",
              summary: `send-to-agent ${reactionKey}`,
              data: { reactionKey, action: "send-to-agent", attempts: tracker.attempts },
            });
            return {
              reactionType: reactionKey,
              success: true,
              action: "send-to-agent",
              message: reactionConfig.message,
              escalated: false,
            };
          } catch (err) {
            // Send failed — allow retry on next poll cycle (don't escalate immediately)
            recordActivityEvent({
              projectId,
              sessionId,
              source: "reaction",
              kind: "reaction.send_to_agent_failed",
              level: "warn",
              summary: `send-to-agent failed for ${sessionId}`,
              data: {
                reactionKey,
                attempts: tracker.attempts,
                errorMessage: err instanceof Error ? err.message : String(err),
              },
            });
            return {
              reactionType: reactionKey,
              success: false,
              action: "send-to-agent",
              escalated: false,
            };
          }
        }
        break;
      }

      case "notify": {
        const context = buildEventContext(session, prEnrichmentCache);
        // A `needs_decision` report parks the session in needs_input, which fires
        // the needs-input notify reaction here. Surface the agent's explicit
        // question + confidence so the human keeps the decision context instead of
        // a generic "needs input" ping (#12). Scoped to the needs-input reactions
        // so unrelated notifications (agent-exited, etc.) are never relabeled.
        const decision =
          (reactionKey === "agent-needs-input" || reactionKey === "report-needs-input") &&
          "lifecycle" in session
            ? readNeedsDecisionReport(session)
            : null;
        const notifyMessage = decision
          ? formatNeedsDecisionMessage(decision)
          : (reactionConfig.message ?? `Reaction '${reactionKey}' triggered notification`);
        const event = createEvent("reaction.triggered", {
          sessionId,
          projectId,
          message: notifyMessage,
          data: buildReactionNotificationData({
            eventType: "reaction.triggered",
            sessionId,
            projectId,
            context,
            reactionKey,
            action: "notify",
            enrichment: getPREnrichmentForSession(session),
          }),
        });
        await notifyHuman(event, reactionConfig.priority ?? "info");
        recordActivityEvent({
          projectId,
          sessionId,
          source: "reaction",
          kind: "reaction.action_succeeded",
          summary: `notify ${reactionKey}`,
          data: { reactionKey, action: "notify", attempts: tracker.attempts },
        });
        return {
          reactionType: reactionKey,
          success: true,
          action: "notify",
          escalated: false,
        };
      }

      case "auto-merge": {
        // Auto-merge is handled by the SCM plugin
        // For now, just notify
        const context = buildEventContext(session, prEnrichmentCache);
        const event = createEvent("reaction.triggered", {
          sessionId,
          projectId,
          message: reactionConfig.message ?? `Reaction '${reactionKey}' triggered auto-merge`,
          data: buildReactionNotificationData({
            eventType: "reaction.triggered",
            sessionId,
            projectId,
            context,
            reactionKey,
            action: "auto-merge",
            enrichment: getPREnrichmentForSession(session),
          }),
        });
        await notifyHuman(event, "action");
        recordActivityEvent({
          projectId,
          sessionId,
          source: "reaction",
          kind: "reaction.action_succeeded",
          summary: `auto-merge ${reactionKey}`,
          data: { reactionKey, action: "auto-merge", attempts: tracker.attempts },
        });
        return {
          reactionType: reactionKey,
          success: true,
          action: "auto-merge",
          escalated: false,
        };
      }

      case "spawn-session": {
        // Trigger a dependency scheduler pass: launch any held sessions whose
        // prerequisites have now merged (#10). This makes "merge unblocks a
        // dependent" configurable as a reaction in addition to the automatic
        // per-poll scheduler.
        const launchedCount = await spawnDependentSessions();
        recordActivityEvent({
          projectId,
          sessionId,
          source: "reaction",
          kind: "reaction.action_succeeded",
          summary: `spawn-session ${reactionKey} (${launchedCount} launched)`,
          data: { reactionKey, action: "spawn-session", launched: launchedCount },
        });
        return {
          reactionType: reactionKey,
          success: true,
          action: "spawn-session",
          escalated: false,
        };
      }
    }

    return {
      reactionType: reactionKey,
      success: false,
      action,
      escalated: false,
    };
  }

  function clearReactionTracker(sessionId: SessionId, reactionKey: string): void {
    reactionTrackers.delete(`${sessionId}:${reactionKey}`);
  }

  function getReactionConfigForSession(
    session: Session,
    reactionKey: string,
  ): ReactionConfig | null {
    const project = config.projects[session.projectId];
    const globalReaction = config.reactions[reactionKey];
    const projectReaction = project?.reactions?.[reactionKey];
    const reactionConfig = projectReaction
      ? { ...globalReaction, ...projectReaction }
      : globalReaction;
    return reactionConfig ? (reactionConfig as ReactionConfig) : null;
  }

  function updateSessionMetadata(session: Session, updates: Partial<Record<string, string>>): void {
    const project = config.projects[session.projectId];
    if (!project) return;

    const sessionsDir = getProjectSessionsDir(session.projectId);
    const lifecycleUpdates = buildLifecycleMetadataPatch(cloneLifecycle(session.lifecycle));
    const mergedUpdates = { ...updates, ...lifecycleUpdates };
    updateMetadata(sessionsDir, session.id, mergedUpdates);
    sessionManager.invalidateCache();

    const cleaned = Object.fromEntries(
      Object.entries(session.metadata).filter(([key]) => {
        const update = mergedUpdates[key];
        return update === undefined || update !== "";
      }),
    );
    for (const [key, value] of Object.entries(mergedUpdates)) {
      if (value === undefined || value === "") continue;
      cleaned[key] = value;
    }
    session.metadata = cleaned;
    session.status = deriveLegacyStatus(session.lifecycle);
  }

  function makeFingerprint(ids: string[]): string {
    return [...ids].sort().join(",");
  }

  /** Parse a comma-joined fingerprint (see makeFingerprint) back into an ID set. */
  function splitFingerprintIds(fingerprint: string): Set<string> {
    return new Set(fingerprint ? fingerprint.split(",") : []);
  }

  /**
   * True when the review reaction for `key` re-delivers comments to the AGENT
   * (an enabled send-to-agent action). Notify-only or auto:false configs surface
   * comments to humans only, so the nudge must not turn them into agent sends.
   */
  function reviewReactionSendsToAgent(session: Session, key: string): boolean {
    const reaction = getReactionConfigForSession(session, key);
    return reaction?.action === "send-to-agent" && reaction.auto !== false;
  }

  /**
   * True when the review reaction for `key` has ESCALATED (exhausted its
   * retries/escalateAfter and handed off to a human). executeReaction records the
   * dispatch hash on escalation even though nothing reached the agent, so the
   * nudge must NOT treat that hash as a delivered backlog and re-send it.
   */
  function isReviewReactionEscalated(session: Session, key: string): boolean {
    return reactionTrackers.get(`${session.id}:${key}`)?.escalated === true;
  }

  /**
   * True when the session has an open PR whose review comments were already
   * delivered to the agent by a send-to-agent reaction. Used only to force a
   * cache-bypassing review-thread fetch while the nudge is actively spending its
   * budget, so a GraphQL-only thread resolution is observed promptly. Notify-only
   * categories are excluded (their dispatch hash is recorded after a human
   * notification, not an agent send).
   */
  function hasDeliveredReviewBacklog(session: Session): boolean {
    if (!session.pr || session.lifecycle.pr.state !== "open") return false;
    const botDelivered =
      !!session.metadata["lastAutomatedReviewDispatchHash"] &&
      reviewReactionSendsToAgent(session, "bugbot-comments");
    const humanDelivered =
      !!session.metadata["lastPendingReviewDispatchHash"] &&
      reviewReactionSendsToAgent(session, "changes-requested");
    return botDelivered || humanDelivered;
  }

  async function maybeDispatchReviewBacklog(
    session: Session,
    _oldStatus: SessionStatus,
    newStatus: SessionStatus,
    transitionReaction?: TransitionReaction,
  ): Promise<void> {
    const project = config.projects[session.projectId];
    if (!project || !session.pr) return;

    const scm = project.scm?.plugin ? registry.get<SCM>("scm", project.scm.plugin) : null;
    if (!scm) return;

    const humanReactionKey = "changes-requested";
    const automatedReactionKey = "bugbot-comments";

    if (TERMINAL_STATUSES.has(newStatus) || session.lifecycle.pr.state !== "open") {
      clearReactionTracker(session.id, humanReactionKey);
      clearReactionTracker(session.id, automatedReactionKey);
      lastReviewBacklogCheckAt.delete(session.id);
      updateSessionMetadata(session, {
        lastPendingReviewFingerprint: "",
        lastPendingReviewDispatchHash: "",
        lastPendingReviewDispatchAt: "",
        lastAutomatedReviewFingerprint: "",
        lastAutomatedReviewDispatchHash: "",
        lastAutomatedReviewDispatchAt: "",
        reviewRoundCount: "",
        // Cumulative churn signal for the confidence gate (#12): reset only here,
        // at a terminal PR state — NOT on bot-review-satisfied — so a later
        // auto-action still scores the rounds this PR actually went through.
        reviewRoundCountTotal: "",
        reviewRoundsEscalated: "",
        reviewSatisfiedAt: "",
        botReviewObserved: "",
        botReviewObservedSha: "",
        stuckNudgeCount: "",
        stuckNudgeEscalated: "",
        stuckNudgeFingerprint: "",
      });
      return;
    }

    // Throttle review backlog API calls to at most once per 2 minutes.
    // Comments don't change faster than this in practice, and the SCM calls
    // (getReviewThreads) consumes API quota on every poll.
    //
    // Exception: bypass throttle when a transition reaction just fired for a
    // review reaction key. The enriched dispatch needs the current fingerprint
    // from the API so it can fire and record the hash in the same cycle. If we
    // throttle here, the next unthrottled poll sees a "new" fingerprint, clears
    // the reaction tracker, and fires a duplicate dispatch.
    // When the agent freshly declares the PR ready for review, force a single
    // re-check so newly resolved/added comment threads are picked up promptly
    // instead of waiting out the 2-minute throttle. Comparing the report
    // timestamp against the last successful fetch ensures exactly one forced
    // re-check per ready-for-review report (the next fetch stamps a newer time).
    const lastCheckAt = lastReviewBacklogCheckAt.get(session.id) ?? 0;
    const agentReport = readAgentReport(session.metadata);
    const readyForReviewRecheck =
      agentReport?.state === "ready_for_review" &&
      isAgentReportFresh(agentReport) &&
      Date.parse(agentReport.timestamp) > lastCheckAt;

    const hasRelevantTransition =
      transitionReaction?.key === humanReactionKey ||
      transitionReaction?.key === automatedReactionKey;
    // A session already marked satisfied bypasses the throttle: reviewSatisfiedAt
    // is the merge-gate clean signal, so a human thread or CI regression must be
    // observed promptly (and the latch cleared) rather than up to 2 minutes late.
    const revalidateSatisfied = !!session.metadata["reviewSatisfiedAt"];
    // While the round-cap latch is set the loop is parked in needs_input. A human
    // may resolve the threads in the UI — a GraphQL-only isResolved change that no
    // REST ETag reflects — so re-check promptly AND force a fresh fetch (below) so
    // the resolution is observed and the loop can unblock.
    const roundEscalated = session.metadata["reviewRoundsEscalated"] === "true";
    // Same rationale as roundEscalated: while a stuck agent is parked in
    // needs_input after exhausting its nudge budget, re-check promptly (and
    // force a fresh fetch below) so the agent/human resolving the comments
    // clears the latch without waiting out the throttle.
    const stuckNudgeEscalated = session.metadata["stuckNudgeEscalated"] === "true";
    // While actively spending the nudge budget on an idle-beyond-threshold agent
    // with already-delivered comments, force a fresh (cache-bypassing) fetch below
    // so a comment the agent resolved via a GraphQL-only isResolved change is seen
    // this cycle rather than counted as still-unresolved — otherwise a stale cache
    // would burn nudges or escalate unnecessarily. This does not bypass the
    // throttle (fetch cadence is unchanged); it only upgrades the fetch to fresh.
    const idleSignalForFetch = session.activitySignal;
    const nudgeInProgress =
      hasPositiveIdleEvidence(idleSignalForFetch) &&
      isIdleBeyondThreshold(session, idleSignalForFetch.timestamp) &&
      hasDeliveredReviewBacklog(session);
    if (
      !hasRelevantTransition &&
      !readyForReviewRecheck &&
      !revalidateSatisfied &&
      !roundEscalated &&
      !stuckNudgeEscalated
    ) {
      if (Date.now() - lastCheckAt < REVIEW_BACKLOG_THROTTLE_MS) {
        return;
      }
    }
    // Force a cache-bypassing fetch when we must observe GraphQL-only thread
    // resolution the REST ETag guards can't see: after the agent reports ready,
    // while the escalation latch waits on a human resolving threads, or when
    // revalidating a satisfied session (an existing thread flipping back to
    // unresolved is a GraphQL-only isResolved change that REST 304s would hide,
    // leaving the merge-ready latch stale).
    const forceFreshFetch =
      readyForReviewRecheck ||
      roundEscalated ||
      revalidateSatisfied ||
      stuckNudgeEscalated ||
      nudgeInProgress;
    // Single GraphQL call for all review threads (human + bot) + review summaries.
    // Split locally by isBot for separate reaction pipelines.
    let allThreads: ReviewComment[];
    let reviewSummaries: ReviewSummary[] = [];
    let currentHeadSha: string | undefined;
    let primaryThreadsTruncated = false;
    try {
      if (scm.getReviewThreads) {
        // Force a fresh fetch when GraphQL-only thread resolution must be seen
        // (agent reported ready, or the escalation latch is waiting on a human).
        const result = await scm.getReviewThreads(session.pr, {
          forceFresh: forceFreshFetch,
        });
        allThreads = result.threads;
        reviewSummaries = result.reviews;
        currentHeadSha = result.headSha;
        primaryThreadsTruncated = result.threadsTruncated ?? false;
      } else {
        // Fallback for SCM plugins that don't implement getReviewThreads yet
        allThreads = await scm.getPendingComments(session.pr);
      }
    } catch (err) {
      // Failed to fetch — preserve existing metadata; record AE evidence so
      // RCA can answer "why aren't review comments being dispatched?"
      recordActivityEvent({
        projectId: session.projectId,
        sessionId: session.id,
        source: "scm",
        kind: "scm.review_fetch_failed",
        level: "warn",
        summary: `review fetch failed for PR #${session.pr.number}`,
        data: {
          plugin: project.scm?.plugin,
          prNumber: session.pr.number,
          prUrl: session.pr.url,
          errorMessage: err instanceof Error ? err.message : String(err),
        },
      });
      // Don't update the throttle timestamp so the next poll retries immediately
      // instead of being blocked for 2 minutes. The agent-stuck notify already
      // fired at the transition (the nudge is purely additive), so a fetch failure
      // only delays the optional comment re-delivery — it never silences an alert.
      return;
    }

    // Only stamp the throttle after a successful SCM fetch. If the fetch failed,
    // we returned above so the next poll can retry without waiting 2 minutes.
    lastReviewBacklogCheckAt.set(session.id, Date.now());

    // Persist review comments + summaries to metadata for dashboard consumption
    {
      const unresolved = allThreads.filter((c) => !c.isBot);
      const reviewBlob = JSON.stringify({
        unresolvedThreads: unresolved.length,
        unresolvedComments: unresolved.map((c) => ({
          url: c.url,
          path: c.path ?? "",
          author: c.author,
          body: c.body,
        })),
        reviews: reviewSummaries.map((r) => ({
          author: r.author,
          state: r.state,
          body: r.body,
        })),
        commentsUpdatedAt: new Date().toISOString(),
      });
      if (session.metadata["prReviewComments"] !== reviewBlob) {
        updateSessionMetadata(session, { prReviewComments: reviewBlob });
      }

      // Persist per-PR review comment blobs for secondary PRs so the dashboard
      // can enrich them independently (prReviewComments_1, prReviewComments_2, …).
      const sessionPRs = normalizeSessionPRs(session);
      const cleanupUpdates = indexedPRMetadataCleanup(session, sessionPRs.length);
      if (Object.keys(cleanupUpdates).length > 0) {
        updateSessionMetadata(session, cleanupUpdates);
      }
      for (let i = 1; i < sessionPRs.length; i++) {
        const secondaryPR = sessionPRs[i];
        if (!secondaryPR) continue;
        let secondaryThreads: ReviewComment[];
        let secondaryReviews: ReviewSummary[];
        try {
          if (scm.getReviewThreads) {
            const result = await scm.getReviewThreads(secondaryPR, {
              forceFresh: forceFreshFetch,
            });
            secondaryThreads = result.threads;
            secondaryReviews = result.reviews;
          } else {
            secondaryThreads = await scm.getPendingComments(secondaryPR);
            secondaryReviews = [];
          }
        } catch {
          continue;
        }
        const secondaryUnresolved = secondaryThreads.filter((c) => !c.isBot);
        const secondaryBlob = JSON.stringify({
          unresolvedThreads: secondaryUnresolved.length,
          unresolvedComments: secondaryUnresolved.map((c) => ({
            url: c.url,
            path: c.path ?? "",
            author: c.author,
            body: c.body,
          })),
          reviews: secondaryReviews.map((r) => ({
            author: r.author,
            state: r.state,
            body: r.body,
          })),
          commentsUpdatedAt: new Date().toISOString(),
        });
        const reviewMetaKey = `prReviewComments_${i}`;
        if (session.metadata[reviewMetaKey] !== secondaryBlob) {
          updateSessionMetadata(session, { [reviewMetaKey]: secondaryBlob });
        }
      }
    }

    const pendingComments = allThreads.filter((c) => !c.isBot);
    const automatedComments = allThreads.filter((c) => c.isBot);

    // Snapshot the dispatch hashes BEFORE the human/bot blocks below mutate them,
    // so the stuck-agent nudge can distinguish "already delivered, agent went
    // idle" (fingerprint == prior dispatch hash) from "fresh comments delivered
    // this cycle" (which the blocks below handle on their own).
    const priorPendingDispatchHash = session.metadata["lastPendingReviewDispatchHash"] ?? "";
    const priorAutomatedDispatchHash = session.metadata["lastAutomatedReviewDispatchHash"] ?? "";

    // Re-validate a previously-recorded "satisfied" mark. Clear it the moment the
    // session stops being demonstrably clean — a reappearing thread, CI no longer
    // green, a changes_requested review, a truncated thread page (can't trust the
    // clean set), or a new head the recorded review no longer covers — so the
    // merge gate never acts on a stale signal. Gated on the mark being set so the
    // extra CI/review checks only run when there is something to invalidate.
    if (session.metadata["reviewSatisfiedAt"]) {
      const anyThreadOpen = pendingComments.length > 0 || automatedComments.length > 0;
      const isMultiPR = normalizeSessionPRs(session).length > 1;
      const ciGreen = await primaryPRCIGreen(session, scm);
      const notChangesRequested = await primaryPRNotChangesRequested(session, scm);
      const headMoved =
        !!currentHeadSha && session.metadata["botReviewObservedSha"] !== currentHeadSha;
      if (
        anyThreadOpen ||
        isMultiPR ||
        !ciGreen ||
        !notChangesRequested ||
        primaryThreadsTruncated ||
        headMoved
      ) {
        updateSessionMetadata(session, { reviewSatisfiedAt: "" });
      }
    }

    // Record that the automated CODE REVIEWER (Codex/Cursor — not CI/coverage
    // bots) has engaged. Completion requires this, so a brand-new PR the reviewer
    // has never touched is not satisfied just because it starts thread-free + CI
    // green. Two signals:
    //  • botReviewObservedSha — the head a review was actually submitted against
    //    (authoritative). A stale review of an older push cannot satisfy a newer
    //    head. Set ONLY from a review submission of the current head, never from
    //    (possibly outdated) inline threads.
    //  • botReviewObserved — a head-agnostic "reviewer touched this PR" boolean,
    //    used only as the fallback when the SCM cannot report a head SHA.
    const reviewedCurrentHead =
      !!currentHeadSha &&
      reviewSummaries.some((r) => r.isReviewBot && r.commitSha === currentHeadSha);
    const anyReviewBotEngagement =
      automatedComments.some((c) => c.isReviewBot) || reviewSummaries.some((r) => r.isReviewBot);
    const engagementUpdates: Record<string, string> = {};
    if (anyReviewBotEngagement && session.metadata["botReviewObserved"] !== "true") {
      engagementUpdates["botReviewObserved"] = "true";
    }
    if (
      reviewedCurrentHead &&
      currentHeadSha &&
      session.metadata["botReviewObservedSha"] !== currentHeadSha
    ) {
      engagementUpdates["botReviewObservedSha"] = currentHeadSha;
    }
    if (Object.keys(engagementUpdates).length > 0) {
      updateSessionMetadata(session, engagementUpdates);
    }

    // --- Pending (human) review comments ---
    {
      const pendingFingerprint = makeFingerprint(pendingComments.map((comment) => comment.id));
      const lastPendingFingerprint = session.metadata["lastPendingReviewFingerprint"] ?? "";
      const lastPendingDispatchHash = session.metadata["lastPendingReviewDispatchHash"] ?? "";

      if (
        pendingFingerprint !== lastPendingFingerprint &&
        transitionReaction?.key !== humanReactionKey
      ) {
        clearReactionTracker(session.id, humanReactionKey);
      }
      if (pendingFingerprint !== lastPendingFingerprint) {
        updateSessionMetadata(session, {
          lastPendingReviewFingerprint: pendingFingerprint,
        });
      }

      if (!pendingFingerprint) {
        clearReactionTracker(session.id, humanReactionKey);
        updateSessionMetadata(session, {
          lastPendingReviewFingerprint: "",
          lastPendingReviewDispatchHash: "",
          lastPendingReviewDispatchAt: "",
        });
      } else if (pendingFingerprint !== lastPendingDispatchHash) {
        const reactionConfig = getReactionConfigForSession(session, humanReactionKey);
        if (
          reactionConfig &&
          reactionConfig.action &&
          (reactionConfig.auto !== false || reactionConfig.action === "notify")
        ) {
          const enrichedMessage = formatReviewCommentsMessage(
            pendingComments,
            "reviewer",
            reviewSummaries,
          );

          // When the transition handler already called executeReaction for this
          // key, send the enriched payload directly to avoid double-billing the
          // reaction attempt budget. A project with retries:1 would otherwise
          // escalate on the very first transition poll.
          // Only bypass for "send-to-agent" — "notify" actions must go through
          // executeReaction so they route to notifyHuman instead of the agent.
          // And NOT when the transition reaction escalated (e.g. a confidence hold
          // or exhausted retries): the direct send would bypass the hold and leak
          // the message to the agent. Routing back through executeReaction lets its
          // `tracker.escalated` guard keep the action held (#12).
          let success = false;
          if (
            transitionReaction?.key === humanReactionKey &&
            reactionConfig.action === "send-to-agent" &&
            !transitionReaction?.result?.escalated
          ) {
            try {
              await sessionManager.send(session.id, enrichedMessage);
              success = true;
            } catch {
              // Send failed — will retry on next unthrottled poll
            }
          } else {
            const enrichedConfig = { ...reactionConfig, message: enrichedMessage };
            const result = await executeReaction(session, humanReactionKey, enrichedConfig);
            // A confidence-held reaction returns escalated (success:true) but
            // delivered nothing to the agent — don't stamp the dispatch hash, or
            // these exact comments get suppressed on later polls (#12 review).
            success = result.success && !result.escalated;
          }
          if (success) {
            updateSessionMetadata(session, {
              lastPendingReviewDispatchHash: pendingFingerprint,
              lastPendingReviewDispatchAt: new Date().toISOString(),
            });
          }
        }
      }
    }

    // --- Automated (bot) review comments — the Codex review→fix loop ---
    {
      const automatedFingerprint = makeFingerprint(automatedComments.map((comment) => comment.id));
      const lastAutomatedFingerprint = session.metadata["lastAutomatedReviewFingerprint"] ?? "";
      const lastAutomatedDispatchHash = session.metadata["lastAutomatedReviewDispatchHash"] ?? "";
      // roundEscalated is read once at the top of maybeDispatchReviewBacklog.
      const reviewRounds = parseAttemptCount(session.metadata["reviewRoundCount"]);
      const maxReviewRounds =
        getReactionConfigForSession(session, automatedReactionKey)?.maxRounds ??
        DEFAULT_MAX_REVIEW_ROUNDS;

      // A genuinely new review round requires new bot content — at least one
      // unresolved comment we have not dispatched before. When the agent merely
      // resolves a subset of an already-dispatched batch, the unresolved set
      // shrinks but no new bot review happened, so this stays false: we neither
      // re-dispatch nor count a new round (which would otherwise let one-at-a-time
      // resolution of a single batch falsely escalate at maxRounds).
      const lastDispatchedBotIds = new Set(
        lastAutomatedDispatchHash ? lastAutomatedDispatchHash.split(",") : [],
      );
      const hasNewBotContent = automatedComments.some((c) => !lastDispatchedBotIds.has(c.id));

      if (automatedFingerprint !== lastAutomatedFingerprint) {
        clearReactionTracker(session.id, automatedReactionKey);
        updateSessionMetadata(session, {
          lastAutomatedReviewFingerprint: automatedFingerprint,
        });
      }

      if (!automatedFingerprint) {
        // No unresolved bot review threads right now. Clear the dispatch-dedup
        // state so a later re-review is dispatched fresh, then run completion
        // detection. NOTE: this does NOT reset reviewRoundCount — see below.
        clearReactionTracker(session.id, automatedReactionKey);
        updateSessionMetadata(session, {
          lastAutomatedReviewFingerprint: "",
          lastAutomatedReviewDispatchHash: "",
          lastAutomatedReviewDispatchAt: "",
        });
        const wasEscalated = session.metadata["reviewRoundsEscalated"] === "true";
        const satisfied = await maybeMarkReviewSatisfied(session, scm, {
          primaryHumanThreadsClear: pendingComments.length === 0,
          headSha: currentHeadSha,
          threadsTruncated: primaryThreadsTruncated,
        });
        // Reset the round budget + escalation latch ONLY when the loop genuinely
        // ends: the review is satisfied, or a human resolved an escalated loop.
        // A mere threads-clear mid-loop (current head not yet reviewed / CI still
        // pending, so maybeMarkReviewSatisfied returned false) must NOT reset the
        // counter — otherwise every review→fix→re-review cycle restarts at round 0
        // and maxReviewRounds could never fire. Invariant preserved: an escalated
        // loop still releases its needs_input latch when a human clears the
        // threads (wasEscalated branch), and the terminal reset above still clears
        // everything when the PR closes/merges.
        if (satisfied || wasEscalated) {
          updateSessionMetadata(session, {
            reviewRoundCount: "",
            reviewRoundsEscalated: "",
          });
        }
      } else if (roundEscalated) {
        // Already escalated after exceeding the round cap. Stay latched — no
        // further auto-dispatch — until the bot threads clear (handled by the
        // branch above) or a human intervenes. determineStatus keeps the
        // session parked in needs_input while this flag is set.
      } else if (hasNewBotContent) {
        // A genuinely new batch of bot review comments = a new review round.
        if (reviewRounds >= maxReviewRounds) {
          await escalateReviewRoundCap(session, reviewRounds, maxReviewRounds);
        } else {
          const reactionConfig = getReactionConfigForSession(session, automatedReactionKey);
          if (
            reactionConfig &&
            reactionConfig.action &&
            (reactionConfig.auto !== false || reactionConfig.action === "notify")
          ) {
            const enrichedMessage = formatReviewCommentsMessage(automatedComments, "bot");

            // Skip the direct-send bypass when the transition reaction escalated
            // (confidence hold or exhausted retries) — route back through
            // executeReaction so its `tracker.escalated` guard keeps the action
            // held instead of leaking the message to the agent (#12).
            let success = false;
            if (
              transitionReaction?.key === automatedReactionKey &&
              reactionConfig.action === "send-to-agent" &&
              !transitionReaction?.result?.escalated
            ) {
              try {
                await sessionManager.send(session.id, enrichedMessage);
                success = true;
              } catch {
                // Send failed — will retry on next unthrottled poll
              }
            } else {
              const enrichedConfig = { ...reactionConfig, message: enrichedMessage };
              const result = await executeReaction(session, automatedReactionKey, enrichedConfig);
              // A confidence-held reaction returns escalated (success:true) but
              // delivered nothing — don't stamp the dispatch hash or count a round,
              // or this batch gets suppressed on later polls (#12 review).
              success = result.success && !result.escalated;
            }
            if (success) {
              updateSessionMetadata(session, {
                lastAutomatedReviewDispatchHash: automatedFingerprint,
                lastAutomatedReviewDispatchAt: new Date().toISOString(),
                reviewRoundCount: String(reviewRounds + 1),
                reviewRoundCountTotal: String(
                  parseAttemptCount(session.metadata["reviewRoundCountTotal"]) + 1,
                ),
              });
            }
          }
        }
      }
    }

    // --- Auto-nudge a stuck/idle agent sitting on already-delivered comments ---
    await maybeNudgeStuckAgent(session, {
      pendingComments,
      automatedComments,
      reviewSummaries,
      priorPendingDispatchHash,
      priorAutomatedDispatchHash,
      primaryThreadsTruncated,
    });
  }

  /**
   * Re-deliver unaddressed PR review comments to a stuck/idle agent, escalating
   * to needs_input after the nudge budget is spent.
   *
   * This is PURELY ADDITIVE to the existing agent-stuck reaction: the transition
   * handler still fires the `agent-stuck` notify exactly as it did before #5 — the
   * nudge NEVER suppresses or defers that notification. The dispatch blocks in
   * maybeDispatchReviewBacklog only re-deliver comments when their fingerprint
   * CHANGES. An agent that is alive but stops noticing already-delivered comments
   * and goes idle leaves the fingerprint unchanged, so the comments would never be
   * re-sent. This re-delivers the outstanding, already-delivered comments
   * (bypassing the fingerprint guard) up to `nudgeRetries` times, then escalates to
   * needs_input so a human takes over.
   *
   * Distinct from runtime-death recovery (packages/core/src/recovery/*), which
   * handles dead processes — this handles alive agents that stopped progressing.
   */
  async function maybeNudgeStuckAgent(
    session: Session,
    ctx: {
      pendingComments: ReviewComment[];
      automatedComments: ReviewComment[];
      reviewSummaries: ReviewSummary[];
      priorPendingDispatchHash: string;
      priorAutomatedDispatchHash: string;
      primaryThreadsTruncated: boolean;
    },
  ): Promise<void> {
    const { pendingComments, automatedComments, reviewSummaries, primaryThreadsTruncated } = ctx;
    const totalUnaddressed = pendingComments.length + automatedComments.length;

    const stuckReaction = getReactionConfigForSession(session, "agent-stuck");
    // Honor `auto: false`: installations that route stuck handling only to
    // humans must not have their agents messaged automatically. The transition
    // handler still emits the human-facing notify for the stuck reaction.
    if (stuckReaction?.auto === false) {
      // …but a nudge latch persisted from a prior config (auto was enabled, the
      // session escalated, then the project was restarted/reconfigured with
      // auto:false) must still be cleared once the backlog is confirmably clean —
      // otherwise determineStatus keeps parking the open PR in needs_input forever
      // after the threads are resolved. Skip sends, but not this cleanup. Fail
      // closed on a truncated page (an unresolved thread may lie outside it).
      if (
        totalUnaddressed === 0 &&
        !primaryThreadsTruncated &&
        (session.metadata["stuckNudgeCount"] ||
          session.metadata["stuckNudgeEscalated"] === "true" ||
          session.metadata["stuckNudgeFingerprint"])
      ) {
        updateSessionMetadata(session, {
          stuckNudgeCount: "",
          stuckNudgeEscalated: "",
          stuckNudgeFingerprint: "",
        });
      }
      return;
    }

    // The bot review→fix loop owns its own escalation / needs_input latch.
    if (session.metadata["reviewRoundsEscalated"] === "true") return;

    // Gate on idle-beyond-threshold, NOT the derived legacy status. An agent that
    // is idle past the agent-stuck threshold but whose PR resolves to an overlay
    // status (changes_requested, review_pending, mergeable, approved, ci_failed)
    // never surfaces as `stuck`, yet it is exactly the alive-but-idle agent this
    // targets. session.activitySignal is the signal determineStatus committed this
    // poll; an actively-working agent has no positive idle evidence.
    const idleSignal = session.activitySignal;
    const stuck =
      hasPositiveIdleEvidence(idleSignal) && isIdleBeyondThreshold(session, idleSignal.timestamp);

    // Backlog confirmably clean (empty AND the fetched page is complete): release
    // the nudge budget + escalation latch so a resolved session leaves needs_input,
    // regardless of idle state (a resumed agent must be released too). Fail closed
    // on a truncated page — an unresolved thread may lie outside the fetched page,
    // so an empty page is NOT trustworthy as clean (fail closed on latch clearing).
    if (totalUnaddressed === 0) {
      if (
        !primaryThreadsTruncated &&
        (session.metadata["stuckNudgeCount"] ||
          session.metadata["stuckNudgeEscalated"] === "true" ||
          session.metadata["stuckNudgeFingerprint"])
      ) {
        updateSessionMetadata(session, {
          stuckNudgeCount: "",
          stuckNudgeEscalated: "",
          stuckNudgeFingerprint: "",
        });
      }
      return;
    }

    // Already escalated (parked in needs_input): don't re-nudge.
    if (session.metadata["stuckNudgeEscalated"] === "true") return;

    // Not idle-beyond-threshold → not stuck; nothing to nudge.
    if (!stuck) return;

    // Preserve each review reaction's own opt-out. The dispatch hash is recorded
    // whenever a category's reaction fires — including notify-only or auto:false
    // configs where the comments were only surfaced to a human, never sent to the
    // agent. Re-delivering those to the agent would bypass the opt-out, so only
    // nudge a category whose reaction is an enabled send-to-agent action.
    //
    // AND skip a category whose review reaction has ESCALATED: executeReaction
    // returns success (recording the dispatch hash) with action "escalated" even
    // though nothing reached the agent, so nudging off that hash would re-send
    // comments the agent never received and fight the reaction's human handoff.
    const pendingSendable =
      reviewReactionSendsToAgent(session, "changes-requested") &&
      !isReviewReactionEscalated(session, "changes-requested");
    const automatedSendable =
      reviewReactionSendsToAgent(session, "bugbot-comments") &&
      !isReviewReactionEscalated(session, "bugbot-comments");

    // Only nudge comments the agent has ALREADY received. Fresh comments were
    // just delivered by the dispatch blocks above this cycle — the normal path
    // handles those. Treat a backlog whose IDs are all a SUBSET of a previously
    // dispatched batch as already-delivered so that partial resolution (e.g. the
    // agent resolved c1 of a dispatched {c1,c2}, leaving {c2}) still nudges and
    // can reach exhaustion escalation, rather than silently receiving nothing.
    const priorPendingIds = splitFingerprintIds(ctx.priorPendingDispatchHash);
    const priorAutomatedIds = splitFingerprintIds(ctx.priorAutomatedDispatchHash);
    const pendingAlreadyDelivered =
      pendingSendable &&
      pendingComments.length > 0 &&
      pendingComments.every((c) => priorPendingIds.has(c.id));
    const automatedAlreadyDelivered =
      automatedSendable &&
      automatedComments.length > 0 &&
      automatedComments.every((c) => priorAutomatedIds.has(c.id));

    // Nothing agent-actionable to re-deliver → nudge does nothing this poll. The
    // transition handler already fired (and owns) the agent-stuck notify.
    if (!pendingAlreadyDelivered && !automatedAlreadyDelivered) return;

    const nudgeRetries = stuckReaction?.nudgeRetries ?? DEFAULT_STUCK_NUDGE_RETRIES;

    // Budget accrues across a shrinking backlog: the count resets to 0 only when
    // genuinely NEW comment IDs appear (fresh work), not when the agent resolves
    // part of the batch — otherwise resolving one comment per poll would refill
    // the budget forever and exhaustion escalation could never fire.
    const currentBacklog = [...pendingComments, ...automatedComments];
    const lastNudgedIds = splitFingerprintIds(session.metadata["stuckNudgeFingerprint"] ?? "");
    const newContentSinceLastNudge = currentBacklog.some((c) => !lastNudgedIds.has(c.id));
    const backlogFingerprint = makeFingerprint(currentBacklog.map((c) => c.id));
    const nudgeCount = newContentSinceLastNudge
      ? 0
      : parseAttemptCount(session.metadata["stuckNudgeCount"]);

    if (nudgeCount >= nudgeRetries) {
      // Nudges exhausted — escalate to needs_input + notify. The latch parks the
      // session in needs_input (honored by determineStatus) until the agent
      // addresses the comments (clearing the backlog above) or a human steps in.
      const context = buildEventContext(session, prEnrichmentCache);
      const event = createEvent("reaction.escalated", {
        sessionId: session.id,
        projectId: session.projectId,
        message: `Agent idle on ${totalUnaddressed} unaddressed review comment(s) after ${nudgeCount} nudge(s) on PR #${session.pr?.number}. Human review needed.`,
        data: buildReactionEscalationNotificationData({
          eventType: "reaction.escalated",
          sessionId: session.id,
          projectId: session.projectId,
          context,
          reactionKey: "agent-stuck",
          action: "escalated",
          attempts: nudgeCount,
          cause: "max_attempts",
          enrichment: getPREnrichmentForSession(session),
        }),
      });
      // Notify FIRST, then latch — and ONLY latch when a notifier accepted the
      // escalation, so we never silently park a session in needs_input with no
      // human aware (mirrors escalateReviewRoundCap).
      const delivered = await notifyHuman(event, stuckReaction?.priority ?? "urgent");
      if (!delivered) return;
      updateSessionMetadata(session, { stuckNudgeEscalated: "true" });
      recordActivityEvent({
        projectId: session.projectId,
        sessionId: session.id,
        source: "reaction",
        kind: "reaction.escalated",
        level: "warn",
        summary: `agent-stuck nudge exhausted (${nudgeCount}) for PR #${session.pr?.number}`,
        data: {
          reactionKey: "agent-stuck",
          nudges: nudgeCount,
          nudgeRetries,
          prNumber: session.pr?.number,
        },
      });
      return;
    }

    // Re-deliver the outstanding comments directly (bypassing the fingerprint
    // guard the dispatch blocks enforce).
    const parts = [
      `You have ${totalUnaddressed} unaddressed review comment(s) on your PR and appear to be idle. Address them now, push fixes, and resolve each thread.`,
      "",
    ];
    if (pendingAlreadyDelivered) {
      parts.push(formatReviewCommentsMessage(pendingComments, "reviewer", reviewSummaries));
    }
    if (automatedAlreadyDelivered) {
      parts.push(formatReviewCommentsMessage(automatedComments, "bot"));
    }

    try {
      await sessionManager.send(session.id, parts.join("\n"));
      updateSessionMetadata(session, {
        stuckNudgeCount: String(nudgeCount + 1),
        stuckNudgeFingerprint: backlogFingerprint,
      });
      recordActivityEvent({
        projectId: session.projectId,
        sessionId: session.id,
        source: "reaction",
        kind: "reaction.action_succeeded",
        summary: `agent-stuck nudge ${nudgeCount + 1}/${nudgeRetries} for PR #${session.pr?.number}`,
        data: {
          reactionKey: "agent-stuck",
          action: "send-to-agent",
          nudge: nudgeCount + 1,
          unaddressed: totalUnaddressed,
        },
      });
    } catch (err) {
      // Send failed — retry on the next poll cycle (don't consume the budget). The
      // agent-stuck notify already fired at the transition, so a human is aware.
      recordActivityEvent({
        projectId: session.projectId,
        sessionId: session.id,
        source: "reaction",
        kind: "reaction.send_to_agent_failed",
        level: "warn",
        summary: `agent-stuck nudge failed for ${session.id}`,
        data: {
          reactionKey: "agent-stuck",
          errorMessage: err instanceof Error ? err.message : String(err),
        },
      });
    }
  }

  /**
   * True when the session's PR has green CI. Reads the batch-enrichment cache
   * first and falls back to a live getCISummary call when enrichment is
   * unavailable (unimplemented plugin or failed batch), so completion detection
   * is not silently disabled under a cache miss. CI that cannot be confirmed
   * green returns false (never satisfy on unknown CI). "none" (no configured
   * checks) counts as green, matching the mergeability path.
   */
  async function primaryPRCIGreen(session: Session, scm: SCM): Promise<boolean> {
    const p = session.pr;
    if (!p) return false;
    const cached = prEnrichmentCache.get(`${p.owner}/${p.repo}#${p.number}`);
    const ci = cached
      ? cached.ciStatus
      : await scm.getCISummary(p).catch(() => undefined); // live fallback
    return ci === "passing" || ci === "none";
  }

  /**
   * True when the session's PR does NOT have a changes_requested review decision.
   * Reads the enrichment cache first (authoritative) and falls back to live
   * getReviewDecision on a cache miss. Fails CLOSED: if the decision cannot be
   * determined (transient/permission error), returns false so a hidden
   * changes_requested cannot produce a false clean signal.
   */
  async function primaryPRNotChangesRequested(session: Session, scm: SCM): Promise<boolean> {
    const p = session.pr;
    if (!p) return false;
    const cached = prEnrichmentCache.get(`${p.owner}/${p.repo}#${p.number}`);
    let decision = cached?.reviewDecision;
    if (!cached) {
      try {
        decision = await scm.getReviewDecision(p); // live fallback
      } catch {
        return false; // fail closed — can't confirm the PR isn't changes_requested
      }
    }
    return decision !== "changes_requested";
  }

  /**
   * Completion detection for the bot review→fix loop: when no unresolved review
   * threads remain, CI is green, no changes_requested review is open, and the
   * code reviewer has reviewed the CURRENT head, record that the review is
   * satisfied so the merge gate (#15) can advance the PR. Latches on
   * `reviewSatisfiedAt`; the latch is cleared (above) when a thread reappears, CI
   * regresses, a change is requested, or the head moves.
   *
   * Multi-PR sessions are deferred to the merge gate (#15): this signal is
   * emitted only for single-PR sessions, so a clean primary can never mask an
   * unreviewed/failing secondary PR.
   */
  async function maybeMarkReviewSatisfied(
    session: Session,
    scm: SCM,
    ctx: { primaryHumanThreadsClear: boolean; headSha: string | undefined; threadsTruncated: boolean },
  ): Promise<boolean> {
    if (session.metadata["reviewSatisfiedAt"]) return false; // already marked
    if (normalizeSessionPRs(session).length > 1) return false; // multi-PR deferred to #15
    if (!ctx.primaryHumanThreadsClear) return false; // human comments unresolved
    // Fail closed when the thread set may be incomplete — an unresolved thread
    // outside the fetched page must not read as "clean".
    if (ctx.threadsTruncated) return false;
    // Require a code-reviewer review of the CURRENT head. The SCM must report a
    // head SHA and the recorded review head must match it — a stale review of an
    // older push does not count. Fail CLOSED when no head SHA is available (e.g.
    // the getPendingComments fallback): a head-agnostic "ever engaged" signal
    // would let a later push be satisfied without a fresh review, so we defer
    // those to the merge gate rather than emit a possibly-stale signal.
    if (!ctx.headSha) return false;
    if (session.metadata["botReviewObservedSha"] !== ctx.headSha) return false;
    // A top-level changes_requested review (even with no inline threads) blocks
    // merge-readiness.
    if (!(await primaryPRNotChangesRequested(session, scm))) return false;
    if (!(await primaryPRCIGreen(session, scm))) return false;

    const satisfiedAt = new Date().toISOString();
    updateSessionMetadata(session, { reviewSatisfiedAt: satisfiedAt });
    recordActivityEvent({
      projectId: session.projectId,
      sessionId: session.id,
      source: "scm",
      kind: "review.satisfied",
      summary: `review satisfied for PR #${session.pr?.number}: no unresolved threads + CI green`,
      data: {
        prNumber: session.pr?.number,
        prUrl: session.pr?.url,
      },
    });

    // Give the completion signal operational effect NOW (not just metadata):
    // emit a first-class, notifiable event so notifiers/reactions can act on the
    // merge-ready state. Auto-merge itself remains the merge gate's job (#15),
    // which consumes reviewSatisfiedAt.
    const context = buildEventContext(session, prEnrichmentCache);
    const event = createEvent("review.satisfied", {
      sessionId: session.id,
      projectId: session.projectId,
      priority: "action",
      message: `Automated review loop complete on PR #${session.pr?.number}: no unresolved threads + CI green — ready to merge.`,
      data: buildPRStateNotificationData({
        eventType: "review.satisfied",
        sessionId: session.id,
        projectId: session.projectId,
        context,
        oldPRState: session.lifecycle.pr.state,
        newPRState: session.lifecycle.pr.state,
        enrichment: getPREnrichmentForSession(session),
      }),
    });
    await notifyHuman(event, "action");
    return true;
  }

  /**
   * Round-cap escalation for the bot review→fix loop. Once `maxRounds` distinct
   * rounds of bot review comments have been dispatched without the threads
   * clearing, stop auto-dispatching and notify a human. The `reviewRoundsEscalated`
   * latch both silences further dispatches and (via determineStatus) parks the
   * session in needs_input until the loop resolves.
   */
  async function escalateReviewRoundCap(
    session: Session,
    rounds: number,
    maxRounds: number,
  ): Promise<void> {
    const context = buildEventContext(session, prEnrichmentCache);
    const event = createEvent("reaction.escalated", {
      sessionId: session.id,
      projectId: session.projectId,
      message: `Automated review→fix loop reached the ${maxRounds}-round cap on PR #${session.pr?.number} without resolving. Human review needed.`,
      data: buildReactionEscalationNotificationData({
        eventType: "reaction.escalated",
        sessionId: session.id,
        projectId: session.projectId,
        context,
        reactionKey: "bugbot-comments",
        action: "escalated",
        attempts: rounds,
        cause: "max_attempts",
        enrichment: getPREnrichmentForSession(session),
      }),
    });
    const reactionConfig = getReactionConfigForSession(session, "bugbot-comments");

    // Notify FIRST, then latch — and ONLY latch when a notifier actually accepted
    // the escalation. notifyHuman catches per-notifier failures (missing target /
    // delivery error) and does not throw, so we must check its delivered result:
    // if nothing was delivered, leave the latch unset so the next poll retries
    // instead of silently parking the session in needs_input with no human aware.
    // Invariant preserved: reviewRoundsEscalated is only ever set once a human has
    // actually been notified, so an escalated (needs_input) session always has a
    // delivered escalation behind it.
    const delivered = await notifyHuman(event, reactionConfig?.priority ?? "urgent");
    if (!delivered) return;

    updateSessionMetadata(session, { reviewRoundsEscalated: "true" });
    recordActivityEvent({
      projectId: session.projectId,
      sessionId: session.id,
      source: "reaction",
      kind: "reaction.escalated",
      level: "warn",
      summary: `bot review loop hit ${maxRounds}-round cap for PR #${session.pr?.number}`,
      data: {
        reactionKey: "bugbot-comments",
        rounds,
        maxRounds,
        prNumber: session.pr?.number,
      },
    });
  }

  /**
   * Format review comments into a message with inline data for the agent.
   * Includes file, line, author, body, and URL so the agent doesn't need
   * to re-fetch via gh api.
   */
  function formatReviewCommentsMessage(
    comments: ReviewComment[],
    source: "reviewer" | "bot",
    reviews: ReviewSummary[] = [],
  ): string {
    const lines: string[] = [];

    // Prepend review summaries (the body submitted with "Changes requested" / "Approve")
    const nonEmptyReviews = reviews.filter((r) => r.body && r.body.trim().length > 0);
    if (nonEmptyReviews.length > 0) {
      for (const r of nonEmptyReviews) {
        lines.push(`Review by @${r.author} (${r.state}):`);
        lines.push(`"${r.body.trim()}"`, "");
      }
    }

    const header =
      source === "reviewer"
        ? `The following ${comments.length} unresolved review comment(s) are on your PR (as of just now). You should not need to re-fetch this data unless you need additional context.`
        : `The following ${comments.length} automated review comment(s) are on your PR (as of just now). You should not need to re-fetch this data unless you need additional context.`;
    lines.push(header, "");
    for (let i = 0; i < comments.length; i++) {
      const c = comments[i];
      const location = c.path ? `${c.path}${c.line ? `:${c.line}` : ""}` : "(general)";
      lines.push(`${i + 1}. ${location} (@${c.author}): "${c.body}"`);
      if (c.url) lines.push(`   ${c.url}`);
      if (c.threadId) lines.push(`   Thread ID: ${c.threadId}`);
    }
    lines.push(
      "",
      "Address each comment, push fixes. Use the thread ID to resolve each thread directly after pushing. You should not need to re-fetch review data unless you need additional context beyond what is provided here.",
    );
    return lines.join("\n");
  }

  function isFailedCICheck(check: CICheck): boolean {
    return check.status === "failed" || check.conclusion?.toUpperCase() === "FAILURE";
  }

  function formatCIFailureSummaryMessage(summary: CIFailureSummary): string {
    const lines = ["CI is failing on your PR.", ""];

    for (const job of summary.failedJobs) {
      const failed = job.failedStep ? `${job.name} → ${job.failedStep}` : job.name;
      lines.push(`Failed: ${failed}`);
      lines.push(`Failure URL: ${job.runUrl}`);

      if (job.logTail) {
        const lineCount = job.logTail.split(/\r?\n/).length;
        const lineLabel = lineCount === 1 ? "line" : "lines";
        const escapedTail = escapeMarkdownCodeFenceClosers(job.logTail);
        lines.push("", `Log tail (last ${lineCount} ${lineLabel}):`, "```", escapedTail, "```");
      }

      lines.push("");
    }

    lines.push("Fix the issues and push again.");
    return lines.join("\n");
  }

  function escapeMarkdownCodeFenceClosers(logTail: string): string {
    return logTail
      .split(/\r?\n/)
      .map((line) => (line.startsWith("```") ? `\u200B${line}` : line))
      .join("\n");
  }

  function formatCIFailureChecksFallback(failedChecks: CICheck[]): string {
    const lines = ["CI checks are failing on your PR. Here are the failed checks:", ""];
    for (const check of failedChecks) {
      const status = check.conclusion ?? check.status;
      const link = check.url ? ` — ${check.url}` : "";
      lines.push(`- **${check.name}**: ${status}${link}`);
    }
    lines.push("", "Investigate the failures, fix the issues, and push again.");
    return lines.join("\n");
  }

  /**
   * Format CI failures into a human-readable message for the agent.
   * Uses SCM-provided failed job/step/log details when available and falls
   * back to check names/statuses/links for SCM plugins that do not implement it.
   */
  async function formatCIFailureMessage(
    scm: SCM,
    pr: PRInfo,
    failedChecks: CICheck[],
  ): Promise<string> {
    if (scm.getCIFailureSummary) {
      try {
        const summary = await scm.getCIFailureSummary(pr, failedChecks);
        if (summary?.failedJobs.length) {
          return formatCIFailureSummaryMessage(summary);
        }
      } catch {
        // Fall back to check names when summary enrichment fails.
      }
    }

    return formatCIFailureChecksFallback(failedChecks);
  }

  async function getFailedCIChecks(
    scm: SCM,
    pr: PRInfo,
    options: { allowFetch: boolean },
  ): Promise<CICheck[] | null> {
    const prKey = `${pr.owner}/${pr.repo}#${pr.number}`;
    const cachedEnrichment = prEnrichmentCache.get(prKey);

    let checks: CICheck[] | undefined = cachedEnrichment?.ciChecks;
    if (checks === undefined && options.allowFetch) {
      try {
        checks = await scm.getCIChecks(pr);
      } catch {
        return null;
      }
    }

    const failedChecks = checks?.filter(isFailedCICheck) ?? [];
    return failedChecks.length > 0 ? failedChecks : null;
  }

  function makeCIFailureFingerprint(failedChecks: CICheck[]): string {
    return makeFingerprint(failedChecks.map((c) => `${c.name}:${c.status}:${c.conclusion ?? ""}`));
  }

  /** Instructions the child agent follows to rebase its branch onto the new
   *  base after its parent merged. A base-edit alone is not enough under the
   *  default squash merge: the child branch still carries the parent's original
   *  commits, so the PR would re-show the parent's changes until the branch is
   *  rebased (dropping those commits) onto the new base. The rebase is delegated
   *  to the agent because it owns the workspace — the daemon must not force-push. */
  function buildStackedRebaseMessage(
    childBranch: string | null,
    oldBase: string,
    targetBase: string,
    prNumbers: number[],
  ): string {
    const branch = childBranch ?? "<your branch>";
    const editHints = prNumbers.map((n) => `gh pr edit ${n} --base ${targetBase}`).join("\n   ");
    const hasPr = prNumbers.length > 0;
    const header = hasPr
      ? `Your stacked PR's parent branch \`${oldBase}\` has merged into \`${targetBase}\`. The orchestrator already pointed your PR base at \`${targetBase}\`, but your branch still carries the parent's now-merged commits — rebase to drop them so your PR shows only your changes:`
      : `Your parent branch \`${oldBase}\` has merged into \`${targetBase}\` and no longer exists. Rebase your branch onto the new base, then open your PR against it:`;
    return [
      header,
      ``,
      `1. \`git fetch origin\``,
      // The old base may only exist as a remote-tracking ref (a child cut from
      // `origin/<base>` never creates a local branch), so resolve a concrete
      // upstream before rebasing rather than assuming a bare local branch name.
      `2. Rebase onto the new base, dropping the parent's now-merged commits:`,
      "   ```",
      `   upstream="$(git rev-parse -q --verify "${oldBase}" || git rev-parse -q --verify "origin/${oldBase}")"`,
      `   git rebase --onto "origin/${targetBase}" "$upstream" ${branch}`,
      "   ```",
      `   (resolve any conflicts, then \`git rebase --continue\`)`,
      `3. \`git push --force-with-lease\``,
      hasPr
        ? `4. Confirm the PR base (no-op if already set):\n   ${editHints}`
        : `4. Open your PR against the new base: \`gh pr create --base ${targetBase} ...\`.`,
    ].join("\n");
  }

  /**
   * Stacked PRs (#11): once a dependent (stacked) session's parent PR has merged,
   * re-point the child's open PRs onto the parent's base AND ask the child agent
   * to rebase its branch onto that base (dropping the parent's merged commits).
   *
   * Child-driven and idempotent: it runs on every poll of the child and latches
   * via `stackRetargetedAt` only after both the base-edit and the agent handoff
   * succeed — so a transient `retargetPR`/`send` failure, or an AO restart after
   * the parent merged, simply retries on the next poll instead of leaving the
   * child stranded on the merged parent branch. GitHub auto-retargets the PR
   * base when the merged head branch is deleted, but that still leaves a wrong
   * diff under squash merge, which is why the agent rebase is required.
   */
  async function maybeRetargetStackedChild(child: Session): Promise<void> {
    const parentSessionId = child.parentSessionId;
    if (!parentSessionId) return;
    if (child.metadata["stackRetargetedAt"]) return; // already handled
    if (TERMINAL_STATUSES.has(child.status)) return; // child itself is wrapping up
    // A held (blocked_by_dependency) child has no runtime yet, so a handoff would
    // just fail-to-send every poll. `unblock()` re-resolves the base off the
    // (now-merged) parent when the scheduler launches it, so skip it here.
    if (isBlockedByDependency(child.lifecycle)) return;
    // NOTE: we intentionally do NOT require an open PR here. A child that hasn't
    // opened its PR yet still needs to hear that the parent merged — its prompt
    // told it to `gh pr create --base <parent>`, and that branch is now gone.

    const project = config.projects[child.projectId];
    if (!project) return;

    // Resolve the parent's merge state and the base it merged into — same rules
    // as the session-manager spawn path (see resolveStackedChildBase).
    let parent: Session | null;
    try {
      parent = await sessionManager.get(parentSessionId);
    } catch {
      parent = null;
    }
    const resolved = resolveStackedChildBase(
      parent
        ? {
            lifecycle: parent.lifecycle,
            branch: parent.branch,
            // The parent's own base: prefer live enrichment, then persisted baseRef.
            ownBase: parent.pr?.baseBranch || parent.metadata["baseRef"],
          }
        : null,
    );
    if (!resolved.parentMerged) return; // parent still open — wait for the merge
    const targetBase = resolved.base || project.defaultBranch;

    // The branch the child currently stacks on (persisted at spawn). Without it
    // we can't compute the `--onto` cut point, so there's nothing safe to do.
    const oldBase = child.metadata["baseRef"] || parent?.branch;
    const nowIso = new Date().toISOString();
    if (!targetBase || !oldBase || oldBase === targetBase) {
      // Already on the target (or nothing resolvable) — latch so we stop polling.
      updateSessionMetadata(child, { stackRetargetedAt: nowIso });
      return;
    }

    const scm = project.scm?.plugin ? registry.get<SCM>("scm", project.scm.plugin) : null;

    // If the child already has open PR(s) but the SCM can't edit a PR base, the
    // daemon cannot move them off the merged/deleted parent branch. Do NOT latch
    // or tell the agent the base was "already moved" — surface the limitation
    // once and leave it retryable.
    if (child.prs.length > 0 && !scm?.retargetPR) {
      if (!child.metadata["stackRetargetUnsupportedAt"]) {
        updateSessionMetadata(child, { stackRetargetUnsupportedAt: nowIso });
        recordActivityEvent({
          projectId: child.projectId,
          sessionId: child.id,
          source: "lifecycle",
          kind: "stacked.retarget_unsupported",
          level: "warn",
          summary: `${child.id}: SCM cannot retarget a PR base; leaving ${child.prs.length} PR(s) retryable`,
          data: { parentSessionId, newBase: targetBase },
        });
      }
      return;
    }

    // Point every open PR at the new base. A transient failure must NOT latch
    // (retry next poll); a divergence — the live base is neither `oldBase` nor
    // `targetBase`, i.e. a human moved it — must NOT be clobbered: surface it and
    // latch so we neither overwrite the human's choice nor keep nagging.
    if (scm?.retargetPR) {
      const retargetPR = scm.retargetPR.bind(scm);
      for (const pr of child.prs) {
        let outcome: PRRetargetOutcome;
        try {
          outcome = await retargetPR(pr, targetBase, oldBase);
        } catch (err) {
          recordActivityEvent({
            projectId: child.projectId,
            sessionId: child.id,
            source: "lifecycle",
            kind: "stacked.pr_retarget_failed",
            level: "warn",
            summary: `failed to retarget ${child.id} PR onto ${targetBase}`,
            data: {
              parentSessionId,
              newBase: targetBase,
              prUrl: pr.url,
              errorMessage: err instanceof Error ? err.message : String(err),
            },
          });
          return; // retry next poll
        }
        if (outcome === "diverged") {
          recordActivityEvent({
            projectId: child.projectId,
            sessionId: child.id,
            source: "lifecycle",
            kind: "stacked.pr_base_diverged",
            level: "warn",
            summary: `${child.id} PR base was moved elsewhere; leaving it and skipping the rebase handoff`,
            data: { parentSessionId, expectedBase: oldBase, newBase: targetBase, prUrl: pr.url },
          });
          // Don't rebase/edit — a human owns the base now. Latch so we surface
          // once and stop nagging; we neither overwrote nor falsely "retargeted".
          updateSessionMetadata(child, { stackRetargetedAt: nowIso });
          return;
        }
      }
    }

    // Delegate the branch rebase to the agent (it owns the workspace). Only latch
    // once the handoff succeeds, so a send failure retries.
    const message = buildStackedRebaseMessage(
      child.branch,
      oldBase,
      targetBase,
      child.prs.map((p) => p.number).filter((n) => n > 0),
    );
    try {
      await sessionManager.send(child.id, message);
    } catch (err) {
      recordActivityEvent({
        projectId: child.projectId,
        sessionId: child.id,
        source: "lifecycle",
        kind: "stacked.rebase_handoff_failed",
        level: "warn",
        summary: `failed to notify ${child.id} to rebase onto ${targetBase}`,
        data: {
          parentSessionId,
          newBase: targetBase,
          errorMessage: err instanceof Error ? err.message : String(err),
        },
      });
      return; // retry next poll
    }

    // Persist the new base so a later retarget of this child's own children
    // targets the right branch, and latch to stop re-processing.
    updateSessionMetadata(child, { stackRetargetedAt: nowIso, baseRef: targetBase });
    recordActivityEvent({
      projectId: child.projectId,
      sessionId: child.id,
      source: "lifecycle",
      kind: "stacked.pr_retargeted",
      summary: `retargeted ${child.id} onto ${targetBase} and asked the agent to rebase`,
      data: { parentSessionId, oldBase, newBase: targetBase },
    });
  }

  async function maybeDispatchCIFailureDetails(
    session: Session,
    _oldStatus: SessionStatus,
    newStatus: SessionStatus,
    transitionReaction?: TransitionReaction,
  ): Promise<void> {
    const project = config.projects[session.projectId];
    if (!project || !session.pr) return;

    const scm = project.scm?.plugin ? registry.get<SCM>("scm", project.scm.plugin) : null;
    if (!scm) return;

    const ciReactionKey = "ci-failed";

    // Clear tracking when PR is closed/merged
    if (newStatus === "merged" || newStatus === "killed") {
      clearReactionTracker(session.id, ciReactionKey);
      updateSessionMetadata(session, {
        lastCIFailureFingerprint: "",
        lastCIFailureDispatchHash: "",
        lastCIFailureDispatchAt: "",
      });
      return;
    }

    // Only dispatch CI details when in ci_failed state
    if (newStatus !== "ci_failed") {
      // CI is no longer failing — clear tracking so next failure is dispatched fresh
      const lastFingerprint = session.metadata["lastCIFailureFingerprint"] ?? "";
      if (lastFingerprint) {
        clearReactionTracker(session.id, ciReactionKey);
        updateSessionMetadata(session, {
          lastCIFailureFingerprint: "",
          lastCIFailureDispatchHash: "",
          lastCIFailureDispatchAt: "",
        });
      }
      return;
    }

    const failedChecks = await getFailedCIChecks(scm, session.pr, { allowFetch: true });
    if (!failedChecks) return;

    const ciFingerprint = makeCIFailureFingerprint(failedChecks);
    const lastCIFingerprint = session.metadata["lastCIFailureFingerprint"] ?? "";
    const lastCIDispatchHash = session.metadata["lastCIFailureDispatchHash"] ?? "";

    // Reset reaction tracker when failure set changes
    if (ciFingerprint !== lastCIFingerprint && transitionReaction?.key !== ciReactionKey) {
      clearReactionTracker(session.id, ciReactionKey);
    }
    if (ciFingerprint !== lastCIFingerprint) {
      updateSessionMetadata(session, {
        lastCIFailureFingerprint: ciFingerprint,
      });
    }

    // If the transition reaction already delivered an enriched agent message,
    // or handled a non-agent action, record the dispatch hash so subsequent
    // polls don't re-send the same failure details. A confidence-HELD transition
    // (result.escalated) is excluded: it only sent the generic escalation
    // question, so the detailed failed-check follow-up below must still run (#12).
    if (
      transitionReaction?.key === ciReactionKey &&
      transitionReaction.result?.success &&
      !transitionReaction.result.escalated &&
      (transitionReaction.messageEnriched === true ||
        transitionReaction.result.action !== "send-to-agent")
    ) {
      updateSessionMetadata(session, {
        lastCIFailureDispatchHash: ciFingerprint,
        lastCIFailureDispatchAt: new Date().toISOString(),
      });
      return;
    }

    // Skip if we already dispatched this exact failure set
    if (ciFingerprint === lastCIDispatchHash) return;

    // Dispatch CI failure details directly via sessionManager.send() rather than
    // executeReaction() to avoid consuming the ci-failed reaction's retry budget.
    // The transition reaction owns escalation; this is a follow-up info delivery.
    const reactionConfig = getReactionConfigForSession(session, ciReactionKey);
    if (
      reactionConfig &&
      reactionConfig.action &&
      (reactionConfig.auto !== false || reactionConfig.action === "notify")
    ) {
      const detailedMessage = await formatCIFailureMessage(scm, session.pr, failedChecks);

      // Confidence gate (#12): this direct dispatch bypasses executeReaction, so
      // an opted-in ci-failed auto-fix must honor the hold here too. When held
      // (already latched, or scores low now), don't auto-message the agent —
      // deliver the detailed failed-check report to the HUMAN so the escalation
      // is actionable, latch the tracker so the transition path stays held, then
      // record the fingerprint so we don't re-notify this same failure set.
      if (reactionConfig.action === "send-to-agent") {
        const ciTracker = reactionTrackers.get(`${session.id}:${ciReactionKey}`);
        if (
          ciTracker?.escalated ||
          (await holdForLowConfidence(
            session,
            ciReactionKey,
            "send-to-agent",
            reactionConfig,
            ciTracker?.attempts ?? 0,
          ))
        ) {
          const latched = reactionTrackers.get(`${session.id}:${ciReactionKey}`) ?? {
            attempts: 0,
            firstTriggered: new Date(),
          };
          latched.escalated = true;
          reactionTrackers.set(`${session.id}:${ciReactionKey}`, latched);
          try {
            const context = buildEventContext(session, prEnrichmentCache);
            const event = createEvent("ci.failing", {
              sessionId: session.id,
              projectId: session.projectId,
              message: detailedMessage,
              data: buildCIFailureNotificationData({
                sessionId: session.id,
                projectId: session.projectId,
                context,
                failedChecks,
              }),
            });
            await notifyHuman(event, reactionConfig.priority ?? "warning");
            updateSessionMetadata(session, {
              lastCIFailureDispatchHash: ciFingerprint,
              lastCIFailureDispatchAt: new Date().toISOString(),
            });
          } catch {
            // Notify failed — retry on next poll (fingerprint not recorded).
          }
          return;
        }
      }

      try {
        if (reactionConfig.action === "send-to-agent") {
          await sessionManager.send(session.id, detailedMessage);
        } else {
          // For "notify" action, send to human notifiers instead
          const context = buildEventContext(session, prEnrichmentCache);
          const event = createEvent("ci.failing", {
            sessionId: session.id,
            projectId: session.projectId,
            message: detailedMessage,
            data: buildCIFailureNotificationData({
              sessionId: session.id,
              projectId: session.projectId,
              context,
              failedChecks,
            }),
          });
          await notifyHuman(event, reactionConfig.priority ?? "warning");
        }

        updateSessionMetadata(session, {
          lastCIFailureDispatchHash: ciFingerprint,
          lastCIFailureDispatchAt: new Date().toISOString(),
        });
      } catch {
        // Send failed — will retry on next poll cycle
      }
    }
  }

  /**
   * Dispatch merge conflict notifications to the agent session.
   * Conflicts are detected from the PR enrichment cache or getMergeability()
   * and dispatched independently of the session status (conflicts can coexist
   * with ci_failed, changes_requested, etc.).
   */
  async function maybeDispatchMergeConflicts(
    session: Session,
    newStatus: SessionStatus,
  ): Promise<void> {
    const project = config.projects[session.projectId];
    if (!project || !session.pr) return;

    const scm = project.scm?.plugin ? registry.get<SCM>("scm", project.scm.plugin) : null;
    if (!scm) return;

    const conflictReactionKey = "merge-conflicts";

    // Clear tracking when PR is no longer open.
    if (session.lifecycle.pr.state !== "open" || newStatus === "killed") {
      clearReactionTracker(session.id, conflictReactionKey);
      updateSessionMetadata(session, {
        lastMergeConflictDispatched: "",
      });
      return;
    }

    // Only check for conflicts on open PRs
    if (
      newStatus !== "pr_open" &&
      newStatus !== "ci_failed" &&
      newStatus !== "review_pending" &&
      newStatus !== "changes_requested" &&
      newStatus !== "approved" &&
      newStatus !== "mergeable"
    ) {
      return;
    }

    // Check for conflicts using cached enrichment data or fallback to individual call.
    // When batch enrichment ran (cachedData is present), use its hasConflicts value
    // to avoid 3 redundant REST calls from getMergeability() — the batch already
    // fetched the mergeable/mergeStateStatus fields via GraphQL.
    const prKey = `${session.pr.owner}/${session.pr.repo}#${session.pr.number}`;
    const cachedData = prEnrichmentCache.get(prKey);

    if (!cachedData) {
      // No batch data — skip this cycle, batch will populate on next cycle (30s)
      return;
    }
    const hasConflicts = cachedData.hasConflicts ?? false;

    const lastDispatched = session.metadata["lastMergeConflictDispatched"] ?? "";

    if (hasConflicts) {
      // Already dispatched for current conflict state — skip
      if (lastDispatched === "true") return;

      const reactionConfig = getReactionConfigForSession(session, conflictReactionKey);
      if (
        reactionConfig &&
        reactionConfig.action &&
        (reactionConfig.auto !== false || reactionConfig.action === "notify")
      ) {
        try {
          // Build enriched config with dynamic base branch message.
          // Preserve "warning" priority from old direct-dispatch code unless
          // the user explicitly set a different priority in their config.
          const enrichedConfig = {
            ...reactionConfig,
            priority: reactionConfig.priority ?? ("warning" as const),
          };
          if (reactionConfig.action === "send-to-agent" && !reactionConfig.message) {
            const baseBranch = session.pr.baseBranch ?? "the default branch";
            const behindNote = cachedData.isBehind ? ` is behind ${baseBranch} and` : "";
            enrichedConfig.message = `Your PR branch${behindNote} has merge conflicts with ${baseBranch}. Rebase your branch on ${baseBranch}, resolve the conflicts, and push. You should not need to call gh for merge status unless you need additional context — this information is current.`;
          }

          const result = await executeReaction(session, conflictReactionKey, enrichedConfig);
          // Only set dedup flag for non-escalated success — escalation hands off
          // to the human, so we must NOT suppress future agent dispatches if the
          // condition recurs after the tracker resets.
          if (result.success && result.action !== "escalated") {
            updateSessionMetadata(session, {
              lastMergeConflictDispatched: "true",
            });
          }
        } catch {
          // Dispatch failed — will retry on next poll cycle
        }
      }
    } else if (lastDispatched === "true") {
      // Conflicts resolved — clear dedup flag and reaction tracker so future
      // conflicts start a fresh incident with a fresh escalation budget.
      updateSessionMetadata(session, {
        lastMergeConflictDispatched: "",
      });
      clearReactionTracker(session.id, conflictReactionKey);
    }
  }

  /** Send a notification to all configured notifiers. */
  /**
   * Deliver an event to the routed notifiers. Returns true when at least one
   * notifier accepted the event (used by callers that must not latch state on a
   * failed/undelivered notification, e.g. the round-cap escalation).
   */
  async function notifyHuman(event: OrchestratorEvent, priority: EventPriority): Promise<boolean> {
    const eventWithPriority = { ...event, priority };
    // Prefer a per-project routing override (preserves a startup-only project's
    // own routing when merged into a global scope), then top-level, then default.
    const project = config.projects[event.projectId];
    const notifierNames =
      project?.notificationRouting?.[priority] ??
      config.notificationRouting[priority] ??
      config.defaults.notifiers;

    let delivered = false;
    for (const name of notifierNames) {
      const target = resolveNotifierTarget(config, name);
      const notifier =
        registry.get<Notifier>("notifier", target.reference) ??
        registry.get<Notifier>("notifier", target.pluginName);
      if (!notifier) {
        recordNotificationDelivery({
          observer,
          event: eventWithPriority,
          target,
          outcome: "failure",
          method: "notify",
          reason: "notifier target not found",
          failureKind: "target_missing",
          recordActivityEvent: true,
        });
        continue;
      }

      try {
        await notifier.notify(eventWithPriority);
        delivered = true;
        recordNotificationDelivery({
          observer,
          event: eventWithPriority,
          target,
          outcome: "success",
          method: "notify",
        });
      } catch (err) {
        recordNotificationDelivery({
          observer,
          event: eventWithPriority,
          target,
          outcome: "failure",
          method: "notify",
          reason: err instanceof Error ? err.message : String(err),
          failureKind: "delivery_failed",
          recordActivityEvent: true,
        });
      }
    }
    return delivered;
  }

  /**
   * When a session's PR is merged, tear down its tmux runtime, remove its
   * worktree, and archive its metadata. Guarded by an idleness check so we
   * don't kill an agent mid-task; deferred cases set `mergedPendingCleanupSince`
   * in metadata and retry on subsequent polls until the agent idles or the
   * grace window elapses.
   */
  async function maybeAutoCleanupOnMerge(session: Session): Promise<void> {
    if (session.status !== SESSION_STATUS.MERGED) return;

    // Merge the per-project lifecycle override FIELD-BY-FIELD over the top-level
    // config (a partial project override only changes the fields it sets, so it
    // can't accidentally re-enable cleanup the user disabled globally). Defaults
    // apply only when neither level sets a field.
    const project = config.projects[session.projectId];
    const projectLifecycle = project?.lifecycle;
    const topLifecycle = config.lifecycle;
    const autoCleanupOnMerge =
      projectLifecycle?.autoCleanupOnMerge ?? topLifecycle?.autoCleanupOnMerge ?? true;
    const graceMs =
      projectLifecycle?.mergeCleanupIdleGraceMs ?? topLifecycle?.mergeCleanupIdleGraceMs ?? 300_000;
    if (!autoCleanupOnMerge) return;

    // Check for idleness: if the agent is still working, defer cleanup.
    const nowIso = new Date().toISOString();
    const pendingSince = session.metadata["mergedPendingCleanupSince"] || nowIso;
    const pendingSinceMs = Date.parse(pendingSince);
    const graceElapsed = Number.isFinite(pendingSinceMs)
      ? Date.now() - pendingSinceMs >= graceMs
      : false;

    const activity = session.activity;
    const agentIsBusy =
      activity === ACTIVITY_STATE.ACTIVE ||
      activity === ACTIVITY_STATE.WAITING_INPUT ||
      activity === ACTIVITY_STATE.BLOCKED;

    if (agentIsBusy && !graceElapsed) {
      if (!session.metadata["mergedPendingCleanupSince"]) {
        updateSessionMetadata(session, { mergedPendingCleanupSince: nowIso });
      }
      observer.recordOperation({
        metric: "lifecycle_poll",
        operation: "lifecycle.merge_cleanup.deferred",
        outcome: "success",
        correlationId: createCorrelationId("lifecycle-merge-cleanup"),
        projectId: session.projectId,
        sessionId: session.id,
        reason: primaryLifecycleReason(session.lifecycle),
        data: { activity, pendingSince, graceMs },
        level: "info",
      });
      recordActivityEvent({
        projectId: session.projectId,
        sessionId: session.id,
        source: "lifecycle",
        kind: "session.auto_cleanup_deferred",
        summary: `auto-cleanup deferred for ${session.id}`,
        data: {
          activity,
          // Elapsed wall-time since cleanup was first deferred. NOT a Unix
          // timestamp — naming it `pendingSinceMs` was misleading (Greptile).
          pendingElapsedMs: Number.isFinite(pendingSinceMs) ? Date.now() - pendingSinceMs : null,
          graceMs,
        },
      });
      return;
    }

    const correlationId = createCorrelationId("lifecycle-merge-cleanup");
    try {
      const result = await sessionManager.kill(session.id, {
        purgeOpenCode: true,
        reason: "pr_merged",
      });
      observer.recordOperation({
        metric: "lifecycle_poll",
        operation: "lifecycle.merge_cleanup.completed",
        outcome: "success",
        correlationId,
        projectId: session.projectId,
        sessionId: session.id,
        reason: primaryLifecycleReason(session.lifecycle),
        data: {
          cleaned: result.cleaned,
          alreadyTerminated: result.alreadyTerminated,
          graceElapsed,
          activity,
        },
        level: "info",
      });
      recordActivityEvent({
        projectId: session.projectId,
        sessionId: session.id,
        source: "lifecycle",
        kind: "session.auto_cleanup_completed",
        summary: `auto-cleanup completed for ${session.id}`,
        data: {
          cleaned: result.cleaned,
          alreadyTerminated: result.alreadyTerminated,
          graceElapsed,
          activity,
        },
      });
      states.delete(session.id);
    } catch (err) {
      // Leave `merged` status in place so the next poll retries. Preserve the
      // deferral marker so idempotent retries don't restart the grace clock.
      if (!session.metadata["mergedPendingCleanupSince"]) {
        updateSessionMetadata(session, { mergedPendingCleanupSince: nowIso });
      }
      const errorMsg = err instanceof Error ? err.message : String(err);
      observer.recordOperation({
        metric: "lifecycle_poll",
        operation: "lifecycle.merge_cleanup.failed",
        outcome: "failure",
        correlationId,
        projectId: session.projectId,
        sessionId: session.id,
        reason: errorMsg,
        level: "warn",
      });
      recordActivityEvent({
        projectId: session.projectId,
        sessionId: session.id,
        source: "lifecycle",
        kind: "session.auto_cleanup_failed",
        level: "error",
        summary: `auto-cleanup failed for ${session.id}`,
        data: { errorMessage: errorMsg },
      });
    }
  }

  /** Sum estimated cost per project across a set of (enriched) sessions. */
  function sumProjectCost(sessions: Session[]): Map<string, number> {
    const totals = new Map<string, number>();
    for (const s of sessions) {
      const cost = s.agentInfo?.cost?.estimatedCostUsd ?? 0;
      if (cost > 0) {
        totals.set(s.projectId, (totals.get(s.projectId) ?? 0) + cost);
      }
    }
    return totals;
  }

  /**
   * Stop an over-budget agent from accruing further cost. We interrupt (cancel
   * the in-flight generation) rather than destroy the runtime: destroying would
   * make the next isAlive probe read the runtime as dead and the reconciler
   * would mark the session terminated (#1735), not paused. Interrupt keeps the
   * agent alive and interactive so a human can raise the cap and resume.
   * Best-effort — runtimes without an interrupt() method are a no-op.
   */
  async function interruptForBudget(session: Session): Promise<boolean> {
    if (!session.runtimeHandle) return false;
    // Resolve the runtime that actually launched this session, not the project's
    // current/default. A session started before a `runtime` config change still
    // lives on its original runtime, so the persisted handle's runtimeName is
    // authoritative; the config value is only a fallback for older handles that
    // never recorded it.
    const project = config.projects[session.projectId];
    const runtimeName =
      session.runtimeHandle.runtimeName || project?.runtime || config.defaults.runtime;
    const runtime = registry.get<Runtime>("runtime", runtimeName);
    if (!runtime?.interrupt) return false;
    try {
      await runtime.interrupt(session.runtimeHandle);
      return true;
    } catch (err) {
      recordActivityEvent({
        projectId: session.projectId,
        sessionId: session.id,
        source: "lifecycle",
        kind: "budget.interrupt_failed",
        level: "warn",
        summary: `failed to interrupt over-budget agent ${session.id}`,
        data: { errorMessage: err instanceof Error ? err.message : String(err) },
      });
      return false;
    }
  }

  /** Poll a single session and handle state transitions. */
  async function checkSession(session: Session, projectCostUsd?: number): Promise<void> {
    // Use tracked state if available; otherwise use the persisted metadata status
    // (not session.status, which list() may have already overwritten for dead runtimes).
    // This ensures transitions are detected after a lifecycle manager restart.
    const tracked = states.get(session.id);
    const oldStatus =
      tracked ?? ((session.metadata?.["status"] as SessionStatus | undefined) || session.status);
    const previousLifecycle = cloneLifecycle(session.lifecycle);
    const previousPRState = session.lifecycle.pr.state;
    const assessment = await determineStatus(session);
    if (assessment.skipMetadataWrite) {
      states.set(session.id, oldStatus);
      return;
    }
    let newStatus = assessment.status;

    // Budget enforcement: pause a session whose estimated cost has crossed a
    // configured cap. On the first breach we interrupt the runtime (see
    // interruptForBudget) so the agent actually stops spending — marking
    // metadata alone would leave it running. Reusing the needs_input path makes
    // the pause sticky and lets the existing transition machinery fire the
    // notification exactly once.
    const alreadyBudgetPaused = !!session.metadata["budgetPausedAt"];
    // A budget cap is about CUMULATIVE spend, which never decreases, so once a
    // session crosses the cap it should be flagged regardless of what it is doing
    // right now. Enforce for any session that could be carrying cost — i.e. any
    // non-terminal session that isn't in a transient pre-work/probe state.
    //
    // This deliberately includes the PR-overlay statuses
    // (pr_open/ci_failed/review_pending/changes_requested/approved/mergeable):
    // deriveLegacyStatus() overlays an open PR onto the session and
    // resolveOpenPRDecision() forces the canonical state to "working" OR "idle"
    // there, and it massages the activity signal too — so gating on canonical
    // state or activity would skip a still-over-budget agent that has opened a
    // PR (e.g. still fixing CI / addressing review). Excluded:
    //   - terminal/cleanup (merged/cleanup/done/killed/terminated/errored) — must
    //     never be re-paused; don't interfere with PR-merge or cleanup lifecycle.
    //   - spawning — no cost accrued yet.
    //   - detecting/stuck — transient probe / escalation states we must not disturb.
    const liveState = session.lifecycle.session.state;
    const activeActivity = session.activitySignal.activity === "active";
    const evaluateBudget =
      !TERMINAL_STATUSES.has(newStatus) &&
      newStatus !== SESSION_STATUS.SPAWNING &&
      newStatus !== SESSION_STATUS.DETECTING &&
      newStatus !== SESSION_STATUS.STUCK;
    if (evaluateBudget) {
      const breach = evaluateBudgetBreach(
        config,
        session,
        projectCostUsd ?? session.agentInfo?.cost?.estimatedCostUsd ?? 0,
      );
      if (breach) {
        const firstPause = !alreadyBudgetPaused;
        // Whether the agent is still actively generating (vs. quiet at a prompt),
        // so we re-interrupt despite the latch. Use the activity probe in
        // addition to the canonical state so a still-generating agent under a PR
        // overlay (canonical idle) is caught. Captured before the canonical
        // state is overwritten to needs_input below.
        const stillActive = liveState === "working" || activeActivity;
        // Stamp the transition once, at first pause, and reuse that timestamp on
        // every subsequent poll while still over budget. Resetting it each poll
        // would make the dashboard/observability show a perpetually-fresh
        // transition time and churn the persisted lifecycle.
        const pausedAt = firstPause
          ? new Date().toISOString()
          : session.metadata["budgetPausedAt"];
        newStatus = SESSION_STATUS.NEEDS_INPUT;
        session.status = SESSION_STATUS.NEEDS_INPUT;
        session.lifecycle.session.state = "needs_input";
        session.lifecycle.session.reason = "awaiting_user_input";
        session.lifecycle.session.lastTransitionAt = pausedAt;
        assessment.evidence = breach.evidence;
        // Actually stop the agent so it can't keep spending tokens while the
        // session is reported paused — marking metadata alone leaves the runtime
        // running. The interrupt can fail transiently (tmux command error,
        // Windows pty-host pipe timeout), so it is latched separately from the
        // pause: retry on every poll until it succeeds. Tying the retry to the
        // pause latch (firstPause) would interrupt exactly once and silently give
        // up on a transient failure, leaving the agent spending indefinitely.
        //
        // The `budgetInterrupted` latch only suppresses re-interrupts for a
        // *quiet* paused session (already stopped at its prompt). If the session
        // is actively generating again while still over the cap (the Escape
        // didn't cancel, or a human resumed the terminal without raising the
        // cap), re-interrupt regardless of the latch — otherwise the agent keeps
        // accruing cost and the budget cap is defeated.
        if (stillActive || session.metadata["budgetInterrupted"] !== "true") {
          const interrupted = await interruptForBudget(session);
          if (interrupted) {
            updateSessionMetadata(session, { budgetInterrupted: "true" });
          }
        }
        if (firstPause) {
          updateSessionMetadata(session, {
            budgetPausedAt: pausedAt,
            budgetPausedReason: breach.evidence,
          });
          recordActivityEvent({
            projectId: session.projectId,
            sessionId: session.id,
            source: "lifecycle",
            kind: "budget.paused",
            level: "warn",
            summary: `paused on ${breach.scope} budget`,
            data: {
              scope: breach.scope,
              limitUsd: breach.limitUsd,
              actualUsd: breach.actualUsd,
            },
          });
        }
      } else if (alreadyBudgetPaused) {
        // No breach reported, but this session was previously paused. Only
        // release the latch when we are sure the breach is genuinely gone —
        // either the cap was removed, or we observed a real cost that is now
        // under it. A transient getSessionInfo/log-read failure leaves
        // agentInfo.cost absent (and sumProjectCost has no entry → projectCostUsd
        // undefined), so evaluateBudgetBreach() sees $0 and reports no breach;
        // treating that as "under budget" would drop the pause and stop
        // retrying interrupts for a session that may still be over the cap.
        const budget = resolveBudget(config, session.projectId);
        const anyCapActive =
          (typeof budget.perSessionUsd === "number" && budget.perSessionUsd > 0) ||
          (typeof budget.perProjectUsd === "number" && budget.perProjectUsd > 0);
        const costObserved =
          projectCostUsd !== undefined ||
          session.agentInfo?.cost?.estimatedCostUsd !== undefined;
        if (!anyCapActive || costObserved) {
          // Cap removed, or a real cost was observed below it — release.
          updateSessionMetadata(session, {
            budgetPausedAt: "",
            budgetPausedReason: "",
            budgetInterrupted: "",
          });
        } else {
          // Cost unobservable this poll — keep the session paused (sticky) and
          // the latch intact, reusing the original pause timestamp, until a real
          // cost reading lets us decide. Re-persist the latch so it survives this
          // poll's metadata write even though we took no breach action.
          const pausedAt = session.metadata["budgetPausedAt"];
          newStatus = SESSION_STATUS.NEEDS_INPUT;
          session.status = SESSION_STATUS.NEEDS_INPUT;
          session.lifecycle.session.state = "needs_input";
          session.lifecycle.session.reason = "awaiting_user_input";
          session.lifecycle.session.lastTransitionAt = pausedAt;
          assessment.evidence = session.metadata["budgetPausedReason"] || assessment.evidence;
          updateSessionMetadata(session, {
            budgetPausedAt: pausedAt,
            budgetPausedReason: session.metadata["budgetPausedReason"] ?? "",
            budgetInterrupted: session.metadata["budgetInterrupted"] ?? "",
          });
        }
      }
    } else if (session.metadata["budgetPausedAt"]) {
      // Session moved on (PR opened, cleaned up, restored): clear the latch so a
      // future breach notifies again.
      updateSessionMetadata(session, {
        budgetPausedAt: "",
        budgetPausedReason: "",
        budgetInterrupted: "",
      });
    }
    const lifecycleChanged = session.metadata["lifecycle"] !== JSON.stringify(session.lifecycle);
    let transitionReaction: TransitionReaction | undefined;

    const nextLifecycleEvidence = assessment.evidence;
    const nextDetectingAttempts =
      assessment.detectingAttempts > 0 ? String(assessment.detectingAttempts) : "";
    const nextDetectingStartedAt = assessment.detectingStartedAt ?? "";
    const nextDetectingEvidenceHash = assessment.detectingEvidenceHash ?? "";
    // Escalation can happen via attempt limit OR time limit
    const isDetectingEscalated =
      newStatus === SESSION_STATUS.STUCK &&
      (assessment.detectingAttempts > DETECTING_MAX_ATTEMPTS ||
        isDetectingTimedOut(nextDetectingStartedAt));
    const nextDetectingEscalatedAt = isDetectingEscalated
      ? session.metadata["detectingEscalatedAt"] || new Date().toISOString()
      : "";

    // Emit ONCE per escalation — guarded by detectingEscalatedAt being empty.
    // Subsequent polls while session stays stuck have detectingEscalatedAt set
    // and won't re-fire (per invariant: don't repeat escalation events).
    if (isDetectingEscalated && !session.metadata["detectingEscalatedAt"]) {
      const cause: "max_attempts" | "max_duration" =
        assessment.detectingAttempts > DETECTING_MAX_ATTEMPTS ? "max_attempts" : "max_duration";
      recordActivityEvent({
        projectId: session.projectId,
        sessionId: session.id,
        source: "lifecycle",
        kind: "detecting.escalated",
        level: "warn",
        summary: `detecting → stuck via ${cause}`,
        data: {
          attempts: assessment.detectingAttempts,
          cause,
          startedAt: nextDetectingStartedAt,
        },
      });
    }

    const metadataUpdates: Record<string, string> = {};
    if (session.metadata["lifecycleEvidence"] !== nextLifecycleEvidence) {
      metadataUpdates["lifecycleEvidence"] = nextLifecycleEvidence;
    }
    if ((session.metadata["detectingAttempts"] || "") !== nextDetectingAttempts) {
      metadataUpdates["detectingAttempts"] = nextDetectingAttempts;
    }
    if ((session.metadata["detectingStartedAt"] || "") !== nextDetectingStartedAt) {
      metadataUpdates["detectingStartedAt"] = nextDetectingStartedAt;
    }
    if ((session.metadata["detectingEvidenceHash"] || "") !== nextDetectingEvidenceHash) {
      metadataUpdates["detectingEvidenceHash"] = nextDetectingEvidenceHash;
    }
    if ((session.metadata["detectingEscalatedAt"] || "") !== nextDetectingEscalatedAt) {
      metadataUpdates["detectingEscalatedAt"] = nextDetectingEscalatedAt;
    }
    if (Object.keys(metadataUpdates).length > 0) {
      updateSessionMetadata(session, metadataUpdates);
    }

    // CI resolution tracking — reset the ci-failed tracker (including its escalated
    // flag) once CI has been passing for CI_PASSING_STABLE_THRESHOLD consecutive polls.
    // This lets the next real CI failure start with a fresh budget.
    if (session.pr) {
      const prKey = `${session.pr.owner}/${session.pr.repo}#${session.pr.number}`;
      const cachedData = prEnrichmentCache.get(prKey);
      if (cachedData) {
        if (cachedData.ciStatus === "passing") {
          const stableCount = Number(session.metadata["ciPassingStableCount"] ?? "0") + 1;
          if (stableCount >= CI_PASSING_STABLE_THRESHOLD) {
            clearReactionTracker(session.id, "ci-failed");
            updateSessionMetadata(session, { ciPassingStableCount: "" });
          } else {
            updateSessionMetadata(session, { ciPassingStableCount: String(stableCount) });
          }
        } else if (session.metadata["ciPassingStableCount"]) {
          // pending or failing resets the stability window — only "passing" counts as resolution
          updateSessionMetadata(session, { ciPassingStableCount: "" });
        }
      }
    }

    if (newStatus !== oldStatus) {
      const correlationId = createCorrelationId("lifecycle-transition");
      // State transition detected
      states.set(session.id, newStatus);
      updateSessionMetadata(session, { status: newStatus });
      recordActivityEvent({
        projectId: session.projectId,
        sessionId: session.id,
        source: "lifecycle",
        kind: "lifecycle.transition",
        level: newStatus === "ci_failed" ? "warn" : "info",
        summary: `${oldStatus} → ${newStatus}`,
        data: { from: oldStatus, to: newStatus },
      });
      observer.recordOperation({
        metric: "lifecycle_poll",
        operation: "lifecycle.transition",
        outcome: "success",
        correlationId,
        projectId: session.projectId,
        sessionId: session.id,
        reason: primaryLifecycleReason(session.lifecycle),
        data: buildTransitionObservabilityData(
          previousLifecycle,
          session.lifecycle,
          oldStatus,
          newStatus,
          assessment.evidence,
          assessment.detectingAttempts,
          true,
        ),
        level: transitionLogLevel(newStatus),
      });

      // Reset allCompleteEmitted when any session becomes active again
      if (!TERMINAL_STATUSES.has(newStatus)) {
        allCompleteEmitted = false;
      }

      // Clear reaction trackers for the old status so retries reset on state changes.
      // Persistent keys (ci-failed) are excluded — their trackers survive oscillation
      // so the escalation budget accumulates across cycles. On escalation, the tracker
      // is cleared in executeReaction so future incidents get a fresh budget.
      const oldEventType = statusToEventType(undefined, oldStatus);
      if (oldEventType) {
        const oldReactionKey = eventToReactionKey(oldEventType);
        if (oldReactionKey && !PERSISTENT_REACTION_KEYS.has(oldReactionKey)) {
          clearReactionTracker(session.id, oldReactionKey);
        }
      }

      // Handle transition: notify humans and/or trigger reactions
      const eventType = statusToEventType(oldStatus, newStatus);
      if (eventType) {
        let reactionHandledNotify = false;
        const reactionKey = eventToReactionKey(eventType);

        if (reactionKey) {
          let reactionConfig = getReactionConfigForSession(session, reactionKey);
          let messageEnriched = false;

          // Enrich CI failure message with failed job/step/log details when
          // batch check data is already available. If it is not, the
          // post-transition CI dispatcher below fetches checks and sends the
          // composed message without altering lifecycle state transitions.
          if (
            reactionKey === "ci-failed" &&
            session.pr &&
            reactionConfig?.action === "send-to-agent"
          ) {
            const project = config.projects[session.projectId];
            const scm = project?.scm?.plugin ? registry.get<SCM>("scm", project.scm.plugin) : null;
            if (scm) {
              const failedChecks = await getFailedCIChecks(scm, session.pr, { allowFetch: false });
              if (failedChecks) {
                reactionConfig = {
                  ...reactionConfig,
                  message: await formatCIFailureMessage(scm, session.pr, failedChecks),
                };
                messageEnriched = true;
              }
            }
          }

          if (reactionConfig && reactionConfig.action) {
            // auto: false skips automated agent actions but still allows notifications
            if (reactionConfig.auto !== false || reactionConfig.action === "notify") {
              const reactionResult = await executeReaction(session, reactionKey, reactionConfig);
              transitionReaction = { key: reactionKey, result: reactionResult, messageEnriched };
              observer.recordOperation({
                metric: "lifecycle_poll",
                operation: "lifecycle.transition.reaction",
                outcome: reactionResult.success ? "success" : "failure",
                correlationId,
                projectId: session.projectId,
                sessionId: session.id,
                reason: primaryLifecycleReason(session.lifecycle),
                data: buildTransitionObservabilityData(
                  previousLifecycle,
                  session.lifecycle,
                  oldStatus,
                  newStatus,
                  assessment.evidence,
                  assessment.detectingAttempts,
                  true,
                  transitionReaction,
                ),
                level: reactionResult.success ? "info" : "warn",
              });
              // Reaction is handling this event — suppress immediate human notification.
              // "send-to-agent" retries + escalates on its own; "notify"/"auto-merge"
              // already call notifyHuman internally. Notifying here would bypass the
              // delayed escalation behaviour configured via retries/escalateAfter.
              reactionHandledNotify = true;
            }
          }
        }

        // For transitions not already notified by a reaction, notify humans.
        // All priorities (including "info") are routed through notificationRouting
        // so the config controls which notifiers receive each priority level.
        if (!reactionHandledNotify) {
          const priority = inferPriority(eventType);
          const context = buildEventContext(session, prEnrichmentCache);
          const event = createEvent(eventType, {
            sessionId: session.id,
            projectId: session.projectId,
            message: `${session.id}: ${oldStatus} → ${newStatus}`,
            data: buildSessionTransitionNotificationData({
              eventType,
              sessionId: session.id,
              projectId: session.projectId,
              context,
              oldStatus,
              newStatus,
              enrichment: getPREnrichmentForSession(session),
            }),
          });
          await notifyHuman(event, priority);
        }
      }
    } else {
      // No transition but track current state
      states.set(session.id, newStatus);
      if (lifecycleChanged) {
        updateSessionMetadata(session, { status: newStatus });
        observer.recordOperation({
          metric: "lifecycle_poll",
          operation: "lifecycle.sync",
          outcome: "success",
          correlationId: createCorrelationId("lifecycle-sync"),
          projectId: session.projectId,
          sessionId: session.id,
          reason: primaryLifecycleReason(session.lifecycle),
          data: buildTransitionObservabilityData(
            previousLifecycle,
            session.lifecycle,
            oldStatus,
            newStatus,
            assessment.evidence,
            assessment.detectingAttempts,
            false,
          ),
          level: transitionLogLevel(newStatus),
        });
      }
    }

    const prEventType = prStateToEventType(previousPRState, session.lifecycle.pr.state);
    if (prEventType) {
      let reactionHandledNotify = false;
      const reactionKey = eventToReactionKey(prEventType);

      if (reactionKey) {
        const reactionConfig = getReactionConfigForSession(session, reactionKey);
        if (reactionConfig && reactionConfig.action) {
          if (reactionConfig.auto !== false || reactionConfig.action === "notify") {
            await executeReaction(session, reactionKey, reactionConfig);
            reactionHandledNotify = true;
          }
        }
      }

      if (!reactionHandledNotify) {
        const context = buildEventContext(session, prEnrichmentCache);
        const prEvent = createEvent(prEventType, {
          sessionId: session.id,
          projectId: session.projectId,
          message: `${session.id}: PR ${previousPRState} → ${session.lifecycle.pr.state}`,
          data: buildPRStateNotificationData({
            eventType: prEventType,
            sessionId: session.id,
            projectId: session.projectId,
            context,
            oldPRState: previousPRState,
            newPRState: session.lifecycle.pr.state,
            enrichment: getPREnrichmentForSession(session),
          }),
        });
        await notifyHuman(prEvent, inferPriority(prEventType));
      }
    }

    // Pin first quality summary for title stability
    if (
      session.agentInfo?.summary &&
      !session.agentInfo.summaryIsFallback &&
      !session.metadata["pinnedSummary"]
    ) {
      const trimmed = session.agentInfo.summary.replace(/[\n\r]/g, " ").trim();
      if (trimmed.length >= 5) {
        try {
          updateSessionMetadata(session, { pinnedSummary: trimmed });
        } catch {
          // Non-critical: title just won't be pinned this cycle
        }
      }
    }

    await Promise.allSettled([
      maybeDispatchReviewBacklog(session, oldStatus, newStatus, transitionReaction),
      maybeDispatchMergeConflicts(session, newStatus),
      maybeDispatchCIFailureDetails(session, oldStatus, newStatus, transitionReaction),
      maybeRetargetStackedChild(session),
    ]);

    // Report watcher: audit agent reports for issues (#140)
    await auditAndReactToReports(session);

    // PR-merge auto-cleanup: tear down runtime + worktree + archive metadata
    // once the agent is idle (or grace window elapses). Runs last so reactions
    // and notifications observe the live session before it is destroyed.
    await maybeAutoCleanupOnMerge(session);
  }

  /**
   * Audit agent reports and trigger reactions when issues are detected.
   * Called at the end of each checkSession cycle.
   */
  async function auditAndReactToReports(session: Session): Promise<void> {
    const auditResult = auditAgentReports(session);
    const now = new Date().toISOString();

    // If no trigger, clear any active trigger metadata
    if (!auditResult || !auditResult.trigger) {
      const hadActiveTrigger = session.metadata[REPORT_WATCHER_METADATA_KEYS.ACTIVE_TRIGGER];
      if (hadActiveTrigger) {
        updateSessionMetadata(session, {
          [REPORT_WATCHER_METADATA_KEYS.LAST_AUDITED_AT]: now,
          [REPORT_WATCHER_METADATA_KEYS.ACTIVE_TRIGGER]: "",
          [REPORT_WATCHER_METADATA_KEYS.TRIGGER_ACTIVATED_AT]: "",
          [REPORT_WATCHER_METADATA_KEYS.TRIGGER_COUNT]: "",
        });
      }
      return;
    }

    const reactionKey = getReactionKeyForTrigger(auditResult.trigger);
    const reactionConfig = getReactionConfigForSession(session, reactionKey);

    // Update audit metadata
    const currentTriggerCount = parseInt(
      session.metadata[REPORT_WATCHER_METADATA_KEYS.TRIGGER_COUNT] ?? "0",
      10,
    );
    // A `needs_decision` report shares the `agent_needs_input` trigger with a
    // generic needs_input, but carries distinct decision context. Fold the
    // reported state AND the report timestamp into the activation identity so
    // that (a) refining needs_input → needs_decision and (b) a SECOND
    // needs_decision with a new question/confidence each count as a NEW
    // activation — re-firing the report reaction / notification even without a
    // status transition. Each `ao report` stamps a fresh timestamp (#12 review).
    const activationIdentity =
      auditResult.report?.state === "needs_decision"
        ? `${auditResult.trigger}:needs_decision:${auditResult.report.timestamp}`
        : auditResult.trigger;
    const isNewTrigger =
      session.metadata[REPORT_WATCHER_METADATA_KEYS.ACTIVE_TRIGGER] !== activationIdentity;

    updateSessionMetadata(session, {
      [REPORT_WATCHER_METADATA_KEYS.LAST_AUDITED_AT]: now,
      [REPORT_WATCHER_METADATA_KEYS.ACTIVE_TRIGGER]: activationIdentity,
      [REPORT_WATCHER_METADATA_KEYS.TRIGGER_ACTIVATED_AT]: isNewTrigger
        ? now
        : (session.metadata[REPORT_WATCHER_METADATA_KEYS.TRIGGER_ACTIVATED_AT] ?? now),
      [REPORT_WATCHER_METADATA_KEYS.TRIGGER_COUNT]: String(
        isNewTrigger ? 1 : currentTriggerCount + 1,
      ),
    });

    // Log the audit finding
    observer.recordOperation({
      metric: "lifecycle_poll",
      operation: "report_watcher.audit",
      outcome: "success",
      correlationId: createCorrelationId("report-watcher"),
      projectId: session.projectId,
      sessionId: session.id,
      reason: auditResult.trigger,
      data: {
        trigger: auditResult.trigger,
        message: auditResult.message,
        timeSinceSpawnMs: auditResult.timeSinceSpawnMs,
        timeSinceReportMs: auditResult.timeSinceReportMs,
        reportState: auditResult.report?.state,
      },
      level: "warn",
    });
    // Emit ONCE per trigger activation (matches the detecting.escalated guard
    // pattern). Without this guard the audit would fire every poll cycle while
    // a trigger stays active, producing hundreds of identical events. The
    // observer.recordOperation above is unguarded by design (it's a metric);
    // the activity-event trail is for actionable evidence, not heartbeat.
    if (isNewTrigger) {
      recordActivityEvent({
        projectId: session.projectId,
        sessionId: session.id,
        source: "report-watcher",
        kind: "report_watcher.triggered",
        level: "warn",
        // Trigger is a bounded enum (no_acknowledge | stale_report |
        // agent_needs_input); auditResult.message includes free-form
        // report.note text from `ao report` and must not land in summary,
        // which is FTS-indexed and only truncated by sanitizeSummary.
        // Full message stays in `data.message` where sanitizeData redacts
        // credential URLs.
        summary: `${auditResult.trigger} triggered`,
        data: {
          trigger: auditResult.trigger,
          message: auditResult.message,
          timeSinceSpawnMs: auditResult.timeSinceSpawnMs,
          timeSinceReportMs: auditResult.timeSinceReportMs,
          reportState: auditResult.report?.state,
        },
      });
    }

    // Execute reaction if configured
    if (isNewTrigger && reactionConfig && reactionConfig.auto !== false) {
      await executeReaction(session, reactionKey, reactionConfig);
    }
  }

  /**
   * Dependency scheduler pass (#10). Runs once per poll over the full session
   * list. When the supervisor is unscoped (the dashboard's portfolio-wide
   * lifecycle worker), `sessions` spans every project, so a backend repo merge
   * can unblock a frontend repo session. For each held (blocked-by-dependency)
   * session:
   *   1. Narrow `blockedBy` by prerequisites whose PR has merged, persisting the
   *      narrowed set immediately so the progress survives both the
   *      prerequisite's post-merge cleanup and an AO restart.
   *   2. Once `blockedBy` is empty, launch the session via `sessionManager.unblock`,
   *      respecting the project's `maxConcurrent` cap.
   * Returns the number of sessions launched this pass.
   */
  async function runScheduler(sessions: Session[]): Promise<number> {
    // `sessions` is an UNSCOPED snapshot (every project) so a merge in one
    // project can satisfy a dependent in another. Held sessions are still
    // narrowed/launched only for this worker's own scope — when the lifecycle
    // worker is project-scoped (the CLI default, lifecycle-service.ts), each
    // worker owns its project's dependents while reading every project's merges.
    // An unscoped worker (scopedProjectId undefined) owns them all. This keeps
    // two project workers from racing to launch the same dependent.
    const held = sessions.filter(
      (s) =>
        isBlockedByDependency(s.lifecycle) &&
        (scopedProjectId === undefined || s.projectId === scopedProjectId),
    );
    if (held.length === 0) return 0;

    const satisfied = collectSatisfiedDependencies(sessions, (projectId) => {
      const proj = config.projects[projectId];
      if (!proj) return undefined;
      return {
        repo: proj.repo,
        // Workspace-scoped trackers (Linear) use globally-unique keys; repo-scoped
        // ones (GitHub/GitLab) keep their bare numeric ids project-local.
        workspaceScopedTracker: proj.tracker?.plugin === "linear",
      };
    });
    // Account for launches issued this pass before the next list() reflects the
    // newly-running sessions, so a burst of unblocks still respects the cap.
    const launchedByProject = new Map<string, number>();
    let launched = 0;

    for (const session of held) {
      const current = session.blockedBy ?? [];
      const remaining = computeRemainingBlockedBy(current, session.projectId, satisfied);

      // Persist any narrowing first — durable regardless of whether the launch
      // below happens now (cap permitting) or on a later poll / after restart.
      if (remaining.length !== current.length) {
        session.blockedBy = remaining;
        updateMetadata(getProjectSessionsDir(session.projectId), session.id, {
          blockedBy: remaining.join(","),
        });
        recordActivityEvent({
          projectId: session.projectId,
          sessionId: session.id,
          source: "lifecycle",
          kind: "scheduler.dependency_resolved",
          summary: `prerequisites resolved for ${session.id}`,
          data: {
            cleared: current.filter((id) => !remaining.includes(id)).join(","),
            remaining: remaining.join(","),
          },
        });
      }

      if (remaining.length > 0) continue;

      // Ready to launch — enforce the per-project concurrency cap.
      const cap = config.projects[session.projectId]?.maxConcurrent;
      if (typeof cap === "number" && cap > 0) {
        const active =
          countActiveSessions(sessions, session.projectId) +
          (launchedByProject.get(session.projectId) ?? 0);
        if (active >= cap) {
          recordActivityEvent({
            projectId: session.projectId,
            sessionId: session.id,
            source: "lifecycle",
            kind: "scheduler.launch_deferred",
            summary: `concurrency cap reached for ${session.projectId} (${active}/${cap})`,
            data: { active, cap },
          });
          continue;
        }
      }

      try {
        await sessionManager.unblock(session.id);
        launched++;
        launchedByProject.set(
          session.projectId,
          (launchedByProject.get(session.projectId) ?? 0) + 1,
        );
        recordActivityEvent({
          projectId: session.projectId,
          sessionId: session.id,
          source: "lifecycle",
          kind: "scheduler.session_launched",
          summary: `launched unblocked session: ${session.id}`,
          data: {},
        });
      } catch (err) {
        recordActivityEvent({
          projectId: session.projectId,
          sessionId: session.id,
          source: "lifecycle",
          kind: "scheduler.launch_failed",
          level: "warn",
          summary: `failed to launch unblocked session: ${session.id}`,
          data: { errorMessage: err instanceof Error ? err.message : String(err) },
        });
      }
    }

    return launched;
  }

  /** Run a scheduler pass against a freshly-listed session set. Backs the
   *  `spawn-session` reaction action so it can be triggered on demand. The list
   *  is UNSCOPED so a merge in one project can unblock a dependent in another;
   *  `runScheduler` still acts only on this worker's scoped dependents. */
  async function spawnDependentSessions(): Promise<number> {
    const sessions = await sessionManager.list();
    return runScheduler(sessions);
  }

  /** Run one polling cycle across all sessions. */
  async function pollAll(): Promise<void> {
    const correlationId = createCorrelationId("lifecycle-poll");
    const startedAt = Date.now();
    // Re-entrancy guard: skip if previous poll is still running
    if (polling) return;
    polling = true;

    try {
      const sessions = await sessionManager.list(scopedProjectId);

      // Include sessions that are active OR whose status changed from what we last saw
      // (e.g., list() detected a dead runtime and marked it "killed" — we need to
      // process that transition even though the new status is terminal)
      const sessionsToCheck = sessions.filter((s) => {
        if (!TERMINAL_STATUSES.has(s.status)) return true;
        const tracked = states.get(s.id);
        return tracked !== undefined && tracked !== s.status;
      });

      await Promise.allSettled(
        sessionsToCheck.map((session) => refreshTrackedBranch(session, sessions)),
      );

      // Prime the per-poll PR enrichment cache before session checks so
      // downstream status/reaction logic can reuse batch GraphQL data.
      await populatePREnrichmentCache(sessionsToCheck);

      // Sum each project's estimated cost across all (enriched) sessions so the
      // per-project budget cap can be evaluated per session below.
      const projectCostUsd = sumProjectCost(sessions);

      // Poll all sessions concurrently. Pass the raw map lookup (possibly
      // undefined) — NOT `?? 0` — so checkSession can tell "no cost observed"
      // (transient enrichment failure) apart from a real $0 aggregate and keep
      // a budget pause latched through the failure window.
      await Promise.allSettled(
        sessionsToCheck.map((s) => checkSession(s, projectCostUsd.get(s.projectId))),
      );

      // Persist batch enrichment data to session metadata files so the
      // web dashboard can read it without calling GitHub API.
      persistPREnrichmentToMetadata(sessionsToCheck);

      // Dependency scheduler (#10): unblock and launch held sessions whose
      // prerequisites just merged. Runs after checkSession so newly-merged
      // PR state is reflected on disk (checkSession persists the merged
      // lifecycle before this point). Uses an UNSCOPED list so a merge in one
      // project can satisfy a dependent in another even when this worker is
      // project-scoped; `runScheduler` still only launches this scope's
      // dependents. Best-effort — a failure here must never abort the poll.
      try {
        const schedulerSessions =
          scopedProjectId === undefined ? sessions : await sessionManager.list();
        await runScheduler(schedulerSessions);
      } catch (err) {
        recordActivityEvent({
          projectId: scopedProjectId,
          source: "lifecycle",
          kind: "scheduler.pass_failed",
          level: "warn",
          summary: "dependency scheduler pass failed",
          data: { errorMessage: err instanceof Error ? err.message : String(err) },
        });
      }

      // Prune stale entries from states, reactionTrackers, and lastReviewBacklogCheckAt
      // for sessions that no longer appear in the session list (e.g., after kill/cleanup)
      const currentSessionIds = new Set(sessions.map((s) => s.id));
      for (const trackedId of states.keys()) {
        if (!currentSessionIds.has(trackedId)) {
          states.delete(trackedId);
        }
      }
      for (const trackedId of activityStateCache.keys()) {
        if (!currentSessionIds.has(trackedId)) {
          activityStateCache.delete(trackedId);
        }
      }
      for (const trackerKey of reactionTrackers.keys()) {
        const sessionId = trackerKey.split(":")[0];
        if (sessionId && !currentSessionIds.has(sessionId)) {
          reactionTrackers.delete(trackerKey);
        }
      }
      for (const sessionId of lastReviewBacklogCheckAt.keys()) {
        if (!currentSessionIds.has(sessionId)) {
          lastReviewBacklogCheckAt.delete(sessionId);
        }
      }

      // Check if all sessions are complete (trigger reaction only once)
      const activeSessions = sessions.filter((s) => !TERMINAL_STATUSES.has(s.status));
      if (sessions.length > 0 && activeSessions.length === 0 && !allCompleteEmitted) {
        allCompleteEmitted = true;

        // Execute all-complete reaction if configured
        const reactionKey = eventToReactionKey("summary.all_complete");
        if (reactionKey) {
          // Per-project worker: honor this project's reaction override (merged over
          // the top-level), so a carried startup-only project's baked all-complete
          // policy applies instead of the unrelated global one. The synthetic
          // system session below doesn't flow through getReactionConfigForSession,
          // so resolve the override explicitly here.
          const allCompleteProject = scopedProjectId ? config.projects[scopedProjectId] : undefined;
          const projectReaction = allCompleteProject?.reactions?.[reactionKey];
          const reactionConfig = projectReaction
            ? { ...config.reactions[reactionKey], ...projectReaction }
            : config.reactions[reactionKey];
          if (reactionConfig && reactionConfig.action) {
            if (reactionConfig.auto !== false || reactionConfig.action === "notify") {
              // Create a minimal session context for system events (no PR/issue context)
              const systemSession: ReactionSessionContext = {
                id: "system" as SessionId,
                projectId: scopedProjectId ?? "all",
                pr: null,
                issueId: null,
                branch: null,
                metadata: {},
                agentInfo: null,
              };
              await executeReaction(systemSession, reactionKey, reactionConfig as ReactionConfig);
            }
          }
        }
      }
      if (scopedProjectId) {
        observer.recordOperation({
          metric: "lifecycle_poll",
          operation: "lifecycle.poll",
          outcome: "success",
          correlationId,
          projectId: scopedProjectId,
          durationMs: Date.now() - startedAt,
          data: { sessionCount: sessions.length, activeSessionCount: activeSessions.length },
          level: "info",
        });
        observer.setHealth({
          surface: "lifecycle.worker",
          status: "ok",
          projectId: scopedProjectId,
          correlationId,
          details: {
            projectId: scopedProjectId,
            sessionCount: sessions.length,
            activeSessionCount: activeSessions.length,
          },
        });
      }
    } catch (err) {
      const errorReason = err instanceof Error ? err.message : String(err);
      observer.recordOperation({
        metric: "lifecycle_poll",
        operation: "lifecycle.poll",
        outcome: "failure",
        correlationId,
        projectId: scopedProjectId,
        durationMs: Date.now() - startedAt,
        reason: errorReason,
        level: "error",
      });
      recordActivityEvent({
        projectId: scopedProjectId,
        source: "lifecycle",
        kind: "lifecycle.poll_failed",
        level: "error",
        // Keep summary generic — sanitizeSummary only truncates, but the FTS
        // index covers it. Error text (which can contain credential URLs from
        // git/gh subprocess output) is routed through `data` where sanitizeData
        // redacts credentials.
        summary: "poll cycle failed",
        data: {
          errorMessage: errorReason,
          durationMs: Date.now() - startedAt,
          projectScope: scopedProjectId ?? "all",
        },
      });
      observer.setHealth({
        surface: "lifecycle.worker",
        status: "error",
        projectId: scopedProjectId,
        correlationId,
        reason: errorReason,
        details: scopedProjectId ? { projectId: scopedProjectId } : { projectScope: "all" },
      });
    } finally {
      polling = false;
    }
  }

  return {
    start(intervalMs = 30_000): void {
      if (pollTimer) return; // Already running
      pollTimer = setInterval(() => void pollAll(), intervalMs);
      // Run immediately on start
      void pollAll();
    },

    stop(): void {
      if (pollTimer) {
        clearInterval(pollTimer);
        pollTimer = null;
      }
    },

    getStates(): Map<SessionId, SessionStatus> {
      return new Map(states);
    },

    async check(sessionId: SessionId): Promise<void> {
      const session = await sessionManager.get(sessionId);
      if (!session) throw new Error(`Session ${sessionId} not found`);
      await refreshTrackedBranch(session);
      // Populate batch enrichment cache for this session's PR so
      // checkSession can read from cache (no individual REST fallback).
      await populatePREnrichmentCache([session]);
      // Sum the project's spend across all its sessions so the per-project
      // budget cap is evaluated correctly for direct (e.g. webhook) checks,
      // not just the polling loop.
      const projectCostUsd = sumProjectCost(await sessionManager.list(session.projectId));
      // Pass the raw lookup (possibly undefined) — see the pollAll call site for
      // why missing cost must not be coalesced to 0.
      await checkSession(session, projectCostUsd.get(session.projectId));
    },
  };
}
