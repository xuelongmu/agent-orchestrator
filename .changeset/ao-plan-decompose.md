---
"@aoagents/ao-core": minor
"@aoagents/ao-cli": minor
---

feat(cli,core): `ao plan` — decompose a goal into linked tickets with an approval gate

Add a planner that turns a high-level goal into a reviewable DAG of linked
tickets and only creates them after human approval:

- `ao plan "<goal>" [--project <id>] [--yes] [--json]` runs a decomposer agent
  headlessly (read-only), parses and validates the structured plan (unique refs,
  resolvable relations, acyclic), renders it for review, and — on confirmation —
  bulk-creates the tickets via the tracker in topological order so blocking and
  parent relations resolve to real issue numbers. Per-ticket `repo` overrides
  route tickets to the correct repository.
- New `core` planner module: `parsePlan`, `validatePlanGraph`, `topoSortPlan`,
  `createPlanTickets`, `decomposeGoal`, plus codex/claude headless runners and a
  `decomposer` agent resolver. The runner is injectable for tests and alternative
  agents.
- Wires the previously-unused `decomposer` config field (`decomposer.agent`,
  falling back to the orchestrator/worker/default agent) and documents it in
  `ao config-help`.
- Teaches the orchestrator prompt when and how to decompose goals with `ao plan`.
