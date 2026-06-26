/**
 * Planner — decompose a high-level goal into a DAG of linked tickets.
 *
 * `ao plan "<goal>"` runs a decomposer agent headlessly (read-only) to emit a
 * structured plan: tickets plus parent/blocking/related relations and an
 * optional target repo per ticket. The plan is presented for human approval;
 * only on approval are the tickets created via the tracker (#7 relations).
 *
 * This module owns the pure pieces — prompt construction, plan parsing/validation
 * (acyclic, references resolve), topological ordering, and bulk creation — plus a
 * default codex-based runner that mirrors the code-review runner. The runner is
 * injectable so the CLI and tests can substitute their own.
 */

import { existsSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { z } from "zod";
import { getShell, isWindows, killProcessTree } from "./platform.js";
import type { CreateIssueInput, Issue, ProjectConfig, Tracker } from "./types.js";

const DECOMPOSER_TIMEOUT_MS = 10 * 60_000;
const DECOMPOSER_MAX_BUFFER = 8 * 1024 * 1024;

// =============================================================================
// Plan schema
// =============================================================================

/**
 * A single planned ticket. `ref` is a plan-local identifier (not a tracker id)
 * used to express relations between tickets that don't exist yet — relations
 * are resolved to real issue numbers at creation time, in topological order.
 */
export const PlannedTicketSchema = z.object({
  /** Plan-local identifier, unique within the plan (e.g. "t1"). */
  ref: z.string().min(1),
  title: z.string().min(1),
  /** Markdown body / description. */
  body: z.string().default(""),
  /** Target repo as "owner/name". Defaults to the project repo when omitted. */
  repo: z.string().min(1).optional(),
  labels: z.array(z.string()).optional(),
  /** Plan-local ref of the parent ticket (sub-issue hierarchy). */
  parentRef: z.string().min(1).optional(),
  /** Plan-local refs of tickets that block this one. */
  blockedByRefs: z.array(z.string().min(1)).optional(),
  /** Plan-local refs of tickets related to this one. */
  relatedRefs: z.array(z.string().min(1)).optional(),
});

export type PlannedTicket = z.infer<typeof PlannedTicketSchema>;

export const PlanSchema = z.object({
  tickets: z.array(PlannedTicketSchema).min(1),
});

export type Plan = z.infer<typeof PlanSchema>;

export class PlanValidationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "PlanValidationError";
  }
}

// =============================================================================
// Parsing & validation
// =============================================================================

function stripMarkdownJsonFence(value: string): string {
  const trimmed = value.trim();
  const match = trimmed.match(/^```(?:json)?\s*([\s\S]*?)\s*```$/i);
  return match?.[1]?.trim() ?? trimmed;
}

/**
 * Extract the first parseable JSON object from raw agent output. The decomposer
 * is instructed to emit only JSON, but agents often wrap it in prose or fences.
 */
function extractJson(raw: string): unknown | null {
  const candidates: string[] = [stripMarkdownJsonFence(raw)];

  const fenced = raw.match(/```(?:json)?\s*([\s\S]*?)\s*```/i);
  if (fenced?.[1]) candidates.push(fenced[1].trim());

  const firstBrace = raw.indexOf("{");
  const lastBrace = raw.lastIndexOf("}");
  if (firstBrace !== -1 && lastBrace > firstBrace) {
    candidates.push(raw.slice(firstBrace, lastBrace + 1));
  }

  for (const candidate of candidates) {
    try {
      return JSON.parse(candidate) as unknown;
    } catch {
      // Keep trying looser candidates.
    }
  }
  return null;
}

/**
 * Parse and validate raw decomposer output into a Plan. Throws
 * PlanValidationError when the output is not valid JSON, fails the schema, or
 * describes a malformed DAG (duplicate refs, dangling references, or a cycle).
 */
export function parsePlan(raw: string): Plan {
  const json = extractJson(raw);
  if (json === null) {
    throw new PlanValidationError("Decomposer did not return valid JSON.");
  }

  const result = PlanSchema.safeParse(json);
  if (!result.success) {
    throw new PlanValidationError(`Plan failed schema validation: ${result.error.message}`);
  }

  validatePlanGraph(result.data);
  return result.data;
}

/**
 * Validate that the plan describes a well-formed DAG: refs are unique, every
 * relation points at a real ref, no ticket references itself, and the
 * parent/blocking graph is acyclic. Returns nothing; throws on the first error.
 */
export function validatePlanGraph(plan: Plan): void {
  const byRef = new Map<string, PlannedTicket>();
  for (const ticket of plan.tickets) {
    if (byRef.has(ticket.ref)) {
      throw new PlanValidationError(`Duplicate ticket ref: "${ticket.ref}".`);
    }
    byRef.set(ticket.ref, ticket);
  }

  const checkRef = (ticket: PlannedTicket, target: string, relation: string): void => {
    if (target === ticket.ref) {
      throw new PlanValidationError(`Ticket "${ticket.ref}" cannot be its own ${relation}.`);
    }
    if (!byRef.has(target)) {
      throw new PlanValidationError(
        `Ticket "${ticket.ref}" references unknown ${relation} "${target}".`,
      );
    }
  };

  for (const ticket of plan.tickets) {
    if (ticket.parentRef) checkRef(ticket, ticket.parentRef, "parent");
    for (const ref of ticket.blockedByRefs ?? []) checkRef(ticket, ref, "blocker");
    for (const ref of ticket.relatedRefs ?? []) checkRef(ticket, ref, "related ticket");
  }

  // Cycle detection over the dependency edges (parent + blockedBy). Related
  // refs are non-directional and intentionally excluded.
  detectCycle(plan, byRef);
}

/** Edges a ticket depends on (must be created first): parent + blockers. */
function dependencyRefs(ticket: PlannedTicket): string[] {
  const refs = [...(ticket.blockedByRefs ?? [])];
  if (ticket.parentRef) refs.push(ticket.parentRef);
  return refs;
}

function detectCycle(plan: Plan, byRef: Map<string, PlannedTicket>): void {
  const VISITING = 1;
  const DONE = 2;
  const marks = new Map<string, number>();

  const visit = (ref: string, stack: string[]): void => {
    const mark = marks.get(ref);
    if (mark === DONE) return;
    if (mark === VISITING) {
      const cycle = [...stack.slice(stack.indexOf(ref)), ref].join(" → ");
      throw new PlanValidationError(`Plan contains a dependency cycle: ${cycle}.`);
    }
    marks.set(ref, VISITING);
    const ticket = byRef.get(ref);
    if (ticket) {
      for (const dep of dependencyRefs(ticket)) {
        visit(dep, [...stack, ref]);
      }
    }
    marks.set(ref, DONE);
  };

  for (const ticket of plan.tickets) visit(ticket.ref, []);
}

/**
 * Order tickets so that every ticket's parent and blockers appear before it.
 * Assumes the plan has already passed validatePlanGraph (acyclic).
 */
export function topoSortPlan(plan: Plan): PlannedTicket[] {
  const byRef = new Map(plan.tickets.map((t) => [t.ref, t]));
  const ordered: PlannedTicket[] = [];
  const placed = new Set<string>();

  const place = (ref: string): void => {
    if (placed.has(ref)) return;
    const ticket = byRef.get(ref);
    if (!ticket) return;
    placed.add(ref); // mark before recursing; graph is known acyclic
    for (const dep of dependencyRefs(ticket)) place(dep);
    ordered.push(ticket);
  };

  for (const ticket of plan.tickets) place(ticket.ref);
  return ordered;
}

// =============================================================================
// Ticket creation
// =============================================================================

export interface CreatedTicket {
  ref: string;
  issue: Issue;
}

export interface CreatePlanTicketsOptions {
  plan: Plan;
  tracker: Tracker;
  /** Base project config. Per-ticket `repo` overrides `project.repo`. */
  project: ProjectConfig;
}

/**
 * Create every ticket in the plan via the tracker, in topological order so that
 * relation references resolve to already-created issue numbers. Per-ticket
 * relations (`parentRef`, `blockedByRefs`, `relatedRefs`) are translated into
 * the tracker's `CreateIssueInput` relation fields.
 */
export async function createPlanTickets({
  plan,
  tracker,
  project,
}: CreatePlanTicketsOptions): Promise<CreatedTicket[]> {
  if (!tracker.createIssue) {
    throw new Error(`Tracker "${tracker.name}" does not support creating issues.`);
  }

  const byRef = new Map(plan.tickets.map((t) => [t.ref, t]));
  // Dependency relations are emulated as repo-local `#N` markers in the issue
  // body, so a parent/blocker in a different repo would resolve to the wrong
  // issue. Reject such edges up front (before creating anything) rather than
  // silently mislinking. (Cross-repo *session* ordering is handled separately
  // by the dependency scheduler.)
  assertDependenciesSameRepo(plan, project, byRef);

  // Related edges are non-directional. Build a symmetric adjacency so the link
  // is recorded on whichever side of the pair is created last (topological
  // order only constrains dependency edges, not related ones).
  const relatedAdjacency = buildRelatedAdjacency(plan);

  const ordered = topoSortPlan(plan);
  const refToIssue = new Map<string, Issue>();
  const created: CreatedTicket[] = [];

  const resolveDep = (ref: string): string => {
    const issue = refToIssue.get(ref);
    if (!issue) {
      // Topological order guarantees parent/blocker dependencies exist first.
      throw new Error(`Internal error: ref "${ref}" was not created before being referenced.`);
    }
    return issue.id;
  };

  for (const ticket of ordered) {
    const ticketProject: ProjectConfig = ticket.repo
      ? { ...project, repo: ticket.repo }
      : project;

    const input: CreateIssueInput = {
      title: ticket.title,
      description: ticket.body,
      ...(ticket.labels && ticket.labels.length > 0 ? { labels: ticket.labels } : {}),
      ...(ticket.parentRef ? { parentId: resolveDep(ticket.parentRef) } : {}),
      ...(ticket.blockedByRefs && ticket.blockedByRefs.length > 0
        ? { blockedBy: ticket.blockedByRefs.map(resolveDep) }
        : {}),
    };

    // Link to every related ticket already created (in either direction). The
    // not-yet-created partner of a forward related edge picks the link up when
    // it is created (it lists this ticket via the symmetric adjacency).
    const relatedIds = [...(relatedAdjacency.get(ticket.ref) ?? [])]
      .map((ref) => refToIssue.get(ref)?.id)
      .filter((id): id is string => Boolean(id));
    if (relatedIds.length > 0) {
      input.relatedTo = relatedIds;
    }

    const issue = await tracker.createIssue(input, ticketProject);
    refToIssue.set(ticket.ref, issue);
    created.push({ ref: ticket.ref, issue });
  }

  return created;
}

/** Effective repo of a ticket: its override, else the project default. */
function effectiveRepo(ticket: PlannedTicket, project: ProjectConfig): string | undefined {
  return ticket.repo ?? project.repo;
}

/**
 * Throw if any dependency edge (parent or blocker) crosses repos. Such edges
 * cannot be faithfully represented by the repo-local `#N` relation emulation.
 */
function assertDependenciesSameRepo(
  plan: Plan,
  project: ProjectConfig,
  byRef: Map<string, PlannedTicket>,
): void {
  for (const ticket of plan.tickets) {
    const ticketRepo = effectiveRepo(ticket, project);
    for (const depRef of dependencyRefs(ticket)) {
      const dep = byRef.get(depRef);
      if (!dep) continue; // validated elsewhere
      const depRepo = effectiveRepo(dep, project);
      if (depRepo !== ticketRepo) {
        throw new PlanValidationError(
          `Cross-repo dependency is not supported: ticket "${ticket.ref}" (${ticketRepo ?? "default repo"}) ` +
            `depends on "${dep.ref}" (${depRepo ?? "default repo"}). ` +
            `Keep blocking/parent relations within a single repo.`,
        );
      }
    }
  }
}

/** Build symmetric related-ref adjacency (each declared edge added both ways). */
function buildRelatedAdjacency(plan: Plan): Map<string, Set<string>> {
  const adjacency = new Map<string, Set<string>>();
  const add = (a: string, b: string): void => {
    let set = adjacency.get(a);
    if (!set) {
      set = new Set();
      adjacency.set(a, set);
    }
    set.add(b);
  };
  for (const ticket of plan.tickets) {
    for (const ref of ticket.relatedRefs ?? []) {
      add(ticket.ref, ref);
      add(ref, ticket.ref);
    }
  }
  return adjacency;
}

// =============================================================================
// Decomposer prompt + runner
// =============================================================================

const PLAN_JSON_SCHEMA_HINT = `{
  "tickets": [
    {
      "ref": "t1",
      "title": "short imperative title",
      "body": "what to do and why, in markdown",
      "repo": "owner/name (optional — omit to use the project repo)",
      "labels": ["optional", "labels"],
      "parentRef": "ref of a parent ticket (optional)",
      "blockedByRefs": ["refs of tickets that must finish first (optional)"],
      "relatedRefs": ["refs of related tickets (optional)"]
    }
  ]
}`;

export interface DecomposerContext {
  goal: string;
  project: ProjectConfig;
  projectId: string;
}

export type PlanRunner = (context: DecomposerContext) => Promise<{ rawOutput: string }>;

export interface DecomposeGoalOptions extends DecomposerContext {
  /** Defaults to the codex-based runner. Injectable for tests / other agents. */
  runPlanner?: PlanRunner;
}

/** Build the prompt handed to the decomposer agent. */
export function buildDecomposerPrompt(context: DecomposerContext): string {
  const { goal, project } = context;
  return [
    "You are an AO planning agent. Decompose the GOAL below into a small set of",
    "well-scoped, independently-shippable tickets. Inspect the repository",
    "(read-only) to ground the plan in the real code.",
    "",
    `GOAL: ${goal}`,
    "",
    `Project: ${project.name}`,
    project.repo ? `Default repo: ${project.repo}` : "Default repo: (none configured)",
    "",
    "Rules:",
    "- Prefer the smallest number of tickets that cleanly covers the goal.",
    "- Express ordering with blockedByRefs (a ticket that depends on another).",
    "- Use parentRef only for genuine sub-issue hierarchies.",
    "- Set repo only when a ticket targets a different repo than the default.",
    "- Do not invent tracker issue numbers — refer to other tickets by their ref.",
    "",
    "Return ONLY a JSON object matching this schema (no prose, no code fences):",
    PLAN_JSON_SCHEMA_HINT,
  ].join("\n");
}

/**
 * Run a process with stdin closed, capturing stdout/stderr with a buffer cap
 * and timeout. Tears down the whole process tree on timeout/overflow (Windows
 * needs this). Mirrors the code-review runner's helper.
 */
async function execFileWithClosedStdin(
  file: string,
  args: string[],
  options: {
    cwd?: string;
    timeout?: number;
    maxBuffer?: number;
    env?: NodeJS.ProcessEnv;
    shell?: boolean | string;
    windowsHide?: boolean;
  } = {},
): Promise<{ stdout: string; stderr: string }> {
  const { spawn } = await import("node:child_process");

  return new Promise((resolve, reject) => {
    const child = spawn(file, args, {
      cwd: options.cwd,
      env: options.env,
      shell: options.shell,
      windowsHide: options.windowsHide ?? true,
      detached: !isWindows(),
      stdio: ["ignore", "pipe", "pipe"],
    });
    const maxBuffer = options.maxBuffer ?? DECOMPOSER_MAX_BUFFER;
    let stdout = "";
    let stderr = "";
    let settled = false;

    const finish = (callback: () => void) => {
      if (settled) return;
      settled = true;
      if (timer) clearTimeout(timer);
      callback();
    };

    const terminateChild = () => {
      if (child.pid !== undefined) {
        return killProcessTree(child.pid).catch(() => undefined);
      }
      child.kill("SIGTERM");
      return Promise.resolve();
    };

    const fail = (message: string, code?: number | null, signal?: NodeJS.Signals | null) => {
      const error = new Error(message) as Error & {
        code?: number | null;
        signal?: NodeJS.Signals | null;
        stdout?: string;
        stderr?: string;
      };
      error.code = code;
      error.signal = signal;
      error.stdout = stdout;
      error.stderr = stderr;
      reject(error);
    };

    const timer =
      options.timeout && options.timeout > 0
        ? setTimeout(() => {
            void terminateChild().finally(() => {
              finish(() => fail(`Command timed out after ${options.timeout}ms`, null, "SIGTERM"));
            });
          }, options.timeout)
        : null;

    const append = (kind: "stdout" | "stderr", chunk: Buffer) => {
      const next = chunk.toString();
      if (kind === "stdout") stdout += next;
      else stderr += next;

      if (Buffer.byteLength(stdout) + Buffer.byteLength(stderr) <= maxBuffer) return;
      void terminateChild();
      finish(() => fail(`Command output exceeded maxBuffer ${maxBuffer}`));
    };

    child.stdout?.on("data", (chunk: Buffer) => append("stdout", chunk));
    child.stderr?.on("data", (chunk: Buffer) => append("stderr", chunk));
    child.once("error", (error) => finish(() => reject(error)));
    child.once("close", (code, signal) => {
      finish(() => {
        if (code === 0) {
          resolve({ stdout, stderr });
          return;
        }
        fail(`Command failed with code ${code ?? signal ?? "unknown"}`, code, signal);
      });
    });
  });
}

/** Build the codex args used to run the decomposer headlessly. */
export function buildCodexDecomposerArgs(outputFile: string, prompt: string): string[] {
  return ["exec", "--sandbox", "read-only", "--output-last-message", outputFile, prompt];
}

/**
 * Default planner: run codex headlessly in read-only mode against the project
 * directory. Mirrors the code-review runner so headless agent invocation stays
 * consistent across features.
 */
export const runCodexDecomposer: PlanRunner = async (context) => {
  const cwd = context.project.path;
  // Capture codex's last message under the OS temp dir (not the user's
  // checkout) so a discarded or --json plan never leaves an untracked file
  // behind. `ao plan` is read-only until tickets are approved.
  const outputFile = join(tmpdir(), `ao-plan-${process.pid}.json`);
  const prompt = buildDecomposerPrompt(context);
  const args = buildCodexDecomposerArgs(outputFile, prompt);

  try {
    const { stdout, stderr } = await execFileWithClosedStdin("codex", args, {
      cwd,
      timeout: DECOMPOSER_TIMEOUT_MS,
      maxBuffer: DECOMPOSER_MAX_BUFFER,
      shell: isWindows(),
    });
    const fileContents = existsSync(outputFile) ? readFileSync(outputFile, "utf-8") : null;
    const rawOutput = fileContents?.trim() || stdout.trim() || stderr.trim();
    return { rawOutput };
  } catch (error) {
    const details =
      error instanceof Error && "stderr" in error && typeof error.stderr === "string"
        ? error.stderr.trim()
        : error instanceof Error
          ? error.message
          : String(error);
    throw new Error(`Decomposer run failed: ${details}`, { cause: error });
  } finally {
    if (existsSync(outputFile)) {
      try {
        rmSync(outputFile, { force: true });
      } catch {
        // Best-effort cleanup of the temp capture file.
      }
    }
  }
};

/** Build the claude args used to run the decomposer headlessly (print mode). */
export function buildClaudeDecomposerArgs(prompt: string): string[] {
  // `--permission-mode plan` keeps the non-interactive run read-only regardless
  // of the project's default Claude permission mode (acceptEdits/bypass/etc.),
  // so `ao plan` cannot edit files before tickets are approved.
  return ["-p", prompt, "--permission-mode", "plan", "--output-format", "text"];
}

/**
 * Claude Code decomposer runner: `claude -p <prompt>` prints the final
 * assistant turn to stdout and exits (headless one-shot).
 */
export const runClaudeDecomposer: PlanRunner = async (context) => {
  const cwd = context.project.path;
  const prompt = buildDecomposerPrompt(context);
  try {
    const { stdout, stderr } = await execFileWithClosedStdin(
      "claude",
      buildClaudeDecomposerArgs(prompt),
      {
        cwd,
        timeout: DECOMPOSER_TIMEOUT_MS,
        maxBuffer: DECOMPOSER_MAX_BUFFER,
        shell: isWindows(),
      },
    );
    return { rawOutput: stdout.trim() || stderr.trim() };
  } catch (error) {
    const details =
      error instanceof Error && "stderr" in error && typeof error.stderr === "string"
        ? error.stderr.trim()
        : error instanceof Error
          ? error.message
          : String(error);
    throw new Error(`Decomposer run failed: ${details}`, { cause: error });
  }
};

/**
 * Resolve which agent `ao plan` should use to decompose, honoring the
 * `decomposer` config field with a sensible fallback chain.
 */
export function resolveDecomposerAgent(
  project: Pick<ProjectConfig, "decomposer" | "orchestrator" | "worker" | "agent">,
  defaults?: {
    decomposer?: { agent?: string };
    orchestrator?: { agent?: string };
    worker?: { agent?: string };
    agent?: string;
  },
): string {
  return (
    project.decomposer?.agent ??
    defaults?.decomposer?.agent ??
    project.orchestrator?.agent ??
    defaults?.orchestrator?.agent ??
    project.worker?.agent ??
    defaults?.worker?.agent ??
    project.agent ??
    defaults?.agent ??
    "claude-code"
  );
}

/**
 * Pick the default headless runner for an agent name. Throws for agents that
 * have no headless planning support so the CLI can surface an actionable error.
 */
export function resolvePlanRunner(agentName: string): PlanRunner {
  switch (agentName) {
    case "codex":
      return runCodexDecomposer;
    case "claude-code":
    case "claude":
      return runClaudeDecomposer;
    default:
      throw new Error(
        `Agent "${agentName}" is not supported for \`ao plan\`. ` +
          `Set decomposer.agent to "codex" or "claude-code".`,
      );
  }
}

/** Build a planner that runs an arbitrary shell command and parses its stdout. */
export function createShellPlanRunner(command: string): PlanRunner {
  return async (context) => {
    const shell = getShell();
    const { stdout, stderr } = await execFileWithClosedStdin(shell.cmd, shell.args(command), {
      cwd: context.project.path,
      timeout: DECOMPOSER_TIMEOUT_MS,
      maxBuffer: DECOMPOSER_MAX_BUFFER,
    });
    return { rawOutput: stdout.trim() || stderr.trim() };
  };
}

/**
 * Run the decomposer for a goal and return a validated Plan. Does NOT create
 * any tickets — the caller presents the plan for approval first.
 */
export async function decomposeGoal(options: DecomposeGoalOptions): Promise<Plan> {
  const { runPlanner = runCodexDecomposer, ...context } = options;
  const { rawOutput } = await runPlanner(context);
  if (!rawOutput) {
    throw new PlanValidationError("Decomposer returned no output.");
  }
  return parsePlan(rawOutput);
}
