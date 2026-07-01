# @aoagents/ao-plugin-tracker-gitlab

## 0.10.0

### Minor Changes

- 669ed4c: feat(cli,core): `ao plan` — decompose a goal into linked tickets with an approval gate

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
  - GitHub and GitLab trackers now render and parse repo-qualified cross-repo
    relation markers (`owner/repo#N`) in issue bodies, which `ao plan` relies on for
    cross-repo blocker ordering. Both tracker packages are bumped so a released CLI
    ships the matching tracker behavior.

- 2d456c4: feat(core,tracker): model parent/child + blocking relations

  Extend the tracker contract with issue hierarchy and dependency relations:
  - `CreateIssueInput` gains `parentId`, `blockedBy`, and `relatedTo`.
  - `Issue` gains `parentId`, `children`, `blockedBy`, `blocks`, and `relatedTo`.
  - Linear sets the parent on create, creates `blocks`/`related` relations via
    `issueRelationCreate`, and returns hierarchy + relations from `getIssue`.
  - GitHub and GitLab emulate relations (best-effort) via a body convention
    (`Part of #N` / `Blocked by #N` / `Related to #N`) that round-trips through
    `getIssue`.

### Patch Changes

- Updated dependencies [669ed4c]
- Updated dependencies [1b9718a]
- Updated dependencies [2d456c4]
- Updated dependencies [c0ef32c]
  - @aoagents/ao-core@0.10.0
  - @aoagents/ao-plugin-scm-gitlab@0.10.0

## 0.9.3

### Patch Changes

- Updated dependencies [cd45a7c]
  - @aoagents/ao-core@0.9.3
  - @aoagents/ao-plugin-scm-gitlab@0.9.3

## 0.9.1

### Patch Changes

- 2d4c457: Fix canary nightly to include all publishable packages and fix Next.js import.meta.url build path issue
- Updated dependencies [2d4c457]
  - @aoagents/ao-core@0.9.1
  - @aoagents/ao-plugin-scm-gitlab@0.9.1

## 0.9.0

### Patch Changes

- Updated dependencies [73bed33]
- Updated dependencies [a610601]
- Updated dependencies [7d9b862]
- Updated dependencies [6d48022]
- Updated dependencies [fcedb25]
- Updated dependencies [94981dc]
- Updated dependencies [2980570]
- Updated dependencies [d5d0f07]
  - @aoagents/ao-core@0.9.0
  - @aoagents/ao-plugin-scm-gitlab@0.9.0

## 0.8.0

### Patch Changes

- Updated dependencies
  - @aoagents/ao-core@0.8.0
  - @aoagents/ao-plugin-scm-gitlab@0.8.0

## 0.7.0

### Patch Changes

- Updated dependencies [0f5ae0b]
- Updated dependencies [fe33bb7]
- Updated dependencies [7c46dc9]
  - @aoagents/ao-core@0.7.0
  - @aoagents/ao-plugin-scm-gitlab@0.7.0

## 0.2.9

### Patch Changes

- Updated dependencies
- Updated dependencies [40aeb78]
- Updated dependencies
- Updated dependencies
  - @aoagents/ao-core@0.6.0
  - @aoagents/ao-plugin-scm-gitlab@0.2.9

## 0.2.8

### Patch Changes

- Updated dependencies [dd07b6b]
  - @aoagents/ao-core@0.5.0
  - @aoagents/ao-plugin-scm-gitlab@0.2.8

## 0.2.7

### Patch Changes

- c8af50f: Make `ProjectConfig.repo` optional to support projects without a configured remote.

  **Migration:** `ProjectConfig.repo` is now `string | undefined` instead of `string`.
  External plugins that access `project.repo` directly (e.g. `project.repo.split("/")`) must
  add a null check first. Use a guard like `if (!project.repo) return null;` or a helper that
  throws with a descriptive error.

- Updated dependencies [2306078]
- Updated dependencies [faaddb1]
- Updated dependencies [f330a1e]
- Updated dependencies [a862327]
- Updated dependencies [331f1ce]
- Updated dependencies [703d584]
- Updated dependencies [f674422]
- Updated dependencies [62353eb]
- Updated dependencies [bd36c7b]
- Updated dependencies [e7ad928]
- Updated dependencies [ca8c4cc]
- Updated dependencies [7b82374]
- Updated dependencies [4701122]
- Updated dependencies [c8af50f]
- Updated dependencies [bcdda4b]
- Updated dependencies [1cbf657]
- Updated dependencies [c447c7c]
- Updated dependencies [a45eb32]
- Updated dependencies [7072143]
- Updated dependencies [ed2dcea]
  - @aoagents/ao-core@0.4.0
  - @aoagents/ao-plugin-scm-gitlab@0.2.7

## 0.1.1

### Patch Changes

- Updated dependencies [3a650b0]
  - @composio/ao-core@0.2.0
  - @composio/ao-plugin-scm-gitlab@0.1.1
