import chalk from "chalk";
import ora from "ora";
import { resolve } from "node:path";
import type { Command } from "commander";
import {
  createPlanTickets,
  decomposeGoal,
  loadConfig,
  recordActivityEvent,
  resolveDecomposerAgent,
  resolvePlanRunner,
  type CreatedTicket,
  type OrchestratorConfig,
  type Plan,
  type PluginRegistry,
  type ProjectConfig,
  type Tracker,
} from "@aoagents/ao-core";
import { getPluginRegistry } from "../lib/create-session-manager.js";
import { findProjectForDirectory } from "../lib/project-resolution.js";
import { promptConfirm } from "../lib/prompts.js";
import { banner } from "../lib/format.js";

/**
 * Resolve the project to plan against. Honors `--project`, then a single
 * configured project, then the project matching the cwd, then AO_PROJECT_ID.
 */
function resolvePlanProject(config: OrchestratorConfig, requested?: string): string {
  const projectIds = Object.keys(config.projects);
  if (projectIds.length === 0) {
    throw new Error("No projects configured. Run 'ao start' first.");
  }

  if (requested) {
    if (config.projects[requested]) return requested;
    throw new Error(
      `Unknown project: ${requested}\nAvailable: ${projectIds.join(", ")}`,
    );
  }

  if (projectIds.length === 1) return projectIds[0];

  const envProject = process.env.AO_PROJECT_ID;
  if (envProject && config.projects[envProject]) return envProject;

  const matched = findProjectForDirectory(config.projects, resolve(process.cwd()));
  if (matched) return matched;

  throw new Error(
    `Multiple projects configured. Specify one with --project <id>: ${projectIds.join(", ")}`,
  );
}

/** Render a plan as an indented tree grouped by dependency order. */
function renderPlan(plan: Plan, project: ProjectConfig): string {
  const lines: string[] = [];
  for (const ticket of plan.tickets) {
    const repo = ticket.repo ?? project.repo ?? "(no repo)";
    lines.push(`  ${chalk.bold(ticket.ref)}  ${ticket.title} ${chalk.dim(`[${repo}]`)}`);
    const relations: string[] = [];
    if (ticket.parentRef) relations.push(`part of ${ticket.parentRef}`);
    if (ticket.blockedByRefs?.length) {
      relations.push(`blocked by ${ticket.blockedByRefs.join(", ")}`);
    }
    if (ticket.relatedRefs?.length) {
      relations.push(`related to ${ticket.relatedRefs.join(", ")}`);
    }
    if (ticket.labels?.length) relations.push(`labels: ${ticket.labels.join(", ")}`);
    if (relations.length > 0) {
      lines.push(`        ${chalk.dim(relations.join(" · "))}`);
    }
  }
  return lines.join("\n");
}

function getTracker(registry: PluginRegistry, project: ProjectConfig): Tracker {
  const trackerName = project.tracker?.plugin;
  if (!trackerName) {
    throw new Error(
      `Project has no tracker configured. Add a 'tracker' block to enable \`ao plan\`.`,
    );
  }
  const tracker = registry.get<Tracker>("tracker", trackerName);
  if (!tracker) {
    throw new Error(`Tracker plugin "${trackerName}" is not available.`);
  }
  if (!tracker.createIssue) {
    throw new Error(`Tracker "${tracker.name}" does not support creating issues.`);
  }
  return tracker;
}

export function registerPlan(program: Command): void {
  program
    .command("plan")
    .description("Decompose a goal into linked tickets, with an approval gate before creation")
    .argument("<goal>", "High-level goal to decompose into tickets")
    .option("--project <id>", "Project to plan against (auto-detected when omitted)")
    .option("--yes", "Skip the approval prompt and create tickets immediately")
    .option("--json", "Print the proposed plan as JSON and exit without creating anything")
    .action(
      async (
        goal: string,
        opts: { project?: string; yes?: boolean; json?: boolean },
      ) => {
        const config = loadConfig();

        let projectId: string;
        try {
          projectId = resolvePlanProject(config, opts.project);
        } catch (err) {
          console.error(chalk.red(err instanceof Error ? err.message : String(err)));
          process.exit(1);
        }

        const project = config.projects[projectId];
        const registry = await getPluginRegistry(config);
        const agentName = resolveDecomposerAgent(project, config.defaults);

        recordActivityEvent({
          projectId,
          source: "cli",
          kind: "cli.plan_invoked",
          level: "info",
          summary: `ao plan invoked`,
          data: { agent: agentName },
        });

        const spinner = ora(`Decomposing goal with ${agentName}`).start();
        let plan: Plan;
        try {
          const runPlanner = resolvePlanRunner(agentName);
          plan = await decomposeGoal({ goal, project, projectId, runPlanner });
          spinner.succeed(`Proposed ${plan.tickets.length} ticket(s)`);
        } catch (err) {
          spinner.fail("Failed to produce a plan");
          recordActivityEvent({
            projectId,
            source: "cli",
            kind: "cli.plan_failed",
            level: "error",
            summary: `ao plan decomposition failed`,
            data: { errorMessage: err instanceof Error ? err.message : String(err) },
          });
          console.error(chalk.red(`✗ ${err instanceof Error ? err.message : String(err)}`));
          process.exit(1);
        }

        // --json only previews the plan — it never creates tickets, so it must
        // not require a tracker that supports issue creation.
        if (opts.json) {
          console.log(JSON.stringify(plan, null, 2));
          return;
        }

        // Resolve the tracker now (creation requires it); deferred past --json
        // so previewing a plan works in projects without a configured tracker.
        let tracker: Tracker;
        try {
          tracker = getTracker(registry, project);
        } catch (err) {
          console.error(chalk.red(`✗ ${err instanceof Error ? err.message : String(err)}`));
          process.exit(1);
        }

        console.log();
        console.log(banner("PROPOSED PLAN"));
        console.log();
        console.log(renderPlan(plan, project));
        console.log();

        if (!opts.yes) {
          const approved = await promptConfirm(
            `Create these ${plan.tickets.length} ticket(s)?`,
            false,
          );
          if (!approved) {
            console.log(chalk.yellow("Plan discarded — no tickets created."));
            return;
          }
        }

        const createSpinner = ora("Creating tickets").start();
        let created: CreatedTicket[];
        try {
          created = await createPlanTickets({ plan, tracker, project });
        } catch (err) {
          createSpinner.fail("Failed to create tickets");
          recordActivityEvent({
            projectId,
            source: "cli",
            kind: "cli.plan_failed",
            level: "error",
            summary: `ao plan ticket creation failed`,
            data: { errorMessage: err instanceof Error ? err.message : String(err) },
          });
          console.error(chalk.red(`✗ ${err instanceof Error ? err.message : String(err)}`));
          process.exit(1);
        }

        createSpinner.succeed(`Created ${created.length} ticket(s)`);
        recordActivityEvent({
          projectId,
          source: "cli",
          kind: "cli.plan_created",
          level: "info",
          summary: `ao plan created ${created.length} tickets`,
          data: { count: created.length },
        });
        for (const { ref, issue } of created) {
          console.log(`  ${chalk.green(issue.id)} ${issue.title} ${chalk.dim(`(${ref}) ${issue.url}`)}`);
        }
      },
    );
}
