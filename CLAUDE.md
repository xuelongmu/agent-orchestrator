# CLAUDE.md

Read and follow [`AGENTS.md`](AGENTS.md) for repository layout, commands, coding conventions, and hard rules.

## Repository skills

| Skill                                                            | Use for                                                                                |
| ---------------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| [`bug-triage`](skills/bug-triage/SKILL.md)                       | Investigating bug reports, checking duplicates, filing issues, and preparing fixes     |
| [`autonomous-drive-loop`](skills/autonomous-drive-loop/SKILL.md) | Operating or recovering durable PR review/fix/merge loops without prompt-carried state |

## App state lives under `~/.ao` only

All app state, the daemon's data dir, `running.json`, worktrees, and the Electron
supervisor's `userData` (Chromium cache, cookies, local/session storage, crash
dumps), must resolve under `~/.ao` (overridable via `AO_DATA_DIR`/`AO_RUN_FILE`).
Never write to or read from `~/Library/Application Support` or any other OS-default
app-data location. `frontend/src/main.ts` pins Electron's `userData` to
`~/.ao/electron`; do not remove that override. See the hard rule in `AGENTS.md`.

Storage is hybrid: durable daemon state, including sessions and activity events,
is stored in SQLite under `AO_DATA_DIR` (default `~/.ao/data`). Managed worktrees
live under `<AO_DATA_DIR>/worktrees`; ephemeral scratch workspaces live under
`<AO_DATA_DIR>/workspaces/scratch`. Directory workspaces reuse the registered
project path and are never removed by AO. `running.json`
and Electron profile data remain files under `~/.ao` by default; transient runtime
state stays in memory.

## Design System

Always read [`DESIGN.md`](DESIGN.md) before making any visual or UI decision —
**start with the "the shipped renderer is the reference" section at the top**,
which governs the current look.

The reference is the code in this repository. Design tokens live in
`frontend/src/styles/tokens.css` (the declared source of truth, imported by
`frontend/src/renderer/styles.css`); structure and behavior live in the shipped
components under `frontend/src/renderer/components`. Change tokens there rather
than hardcoding values. Build new UI from shadcn primitives
(`frontend/src/renderer/components/ui/*`) where a component fits. Do not deviate
without explicit user approval.

Older revisions of DESIGN.md pointed at a private external checkout
(`~/Projects/agent-orchestrator/packages/web/src`) and at an "emdash" framing.
Both are retired: the clone is complete and its palette is preserved in
`tokens.css`. Do not re-flag either in QA/review.

When showing or demoing frontend changes, run `ao preview [url]` from inside the
session so the change renders in the desktop browser panel (the inspector rail's
Browser tab); do not just describe it.
