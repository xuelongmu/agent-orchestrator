# Design System — Agent Orchestrator

> Source of truth for the Agent Orchestrator desktop UI (Electron + React 19 +
> Tailwind v4 + Radix/shadcn + xterm, in `frontend/src/renderer`). Read this
> before any visual or UI change. Created by `/design-consultation` on
> 2026-06-09.

## Design direction — the shipped renderer is the reference

The renderer was originally built by cloning an external web app, and that clone
is **done**. The result now lives in this repository, so the reference is the
code here — not any external checkout:

- **Design tokens:** `frontend/src/styles/tokens.css`. It declares itself the
  source of truth for the visual system and is imported by
  `frontend/src/renderer/styles.css`. Change colors, spacing, radii, and
  typography there; do not hardcode hex values in components.
- **Structure and behavior:** the shipped components under
  `frontend/src/renderer/components`.
- **Build with shadcn primitives** where a component fits
  (`frontend/src/renderer/components/ui/*`: dropdown-menu, select, card, table,
  tooltip, …).

In QA and review, flag divergence from those files. Sections further down this
document that predate the clone are retained for history; where they conflict
with `tokens.css`, **`tokens.css` wins**.

> **Historical note.** Earlier revisions of this file directed contributors to
> clone `~/Projects/agent-orchestrator/packages/web/src` "verbatim" and described
> the design as "matching emdash". That path was a private checkout belonging to
> the original author of this document and is not available to anyone else, so
> the instruction was unfollowable. The palette it specified survives intact in
> `tokens.css` (near-black surface ramp, hairline white-alpha borders,
> refined-blue `#4d8dff` accent, and the working/needs-you/mergeable/fail status
> colors), which is why that reference is now retired rather than reconstructed.

### Palette

Authoritative values live in `frontend/src/styles/tokens.css`; this summary is
for orientation only. Surfaces `--color-bg-primary #0a0b0d`,
`--color-bg-secondary #15171b`, sidebar rail `--color-bg-sidebar #08090b`; text
`--color-text-primary #f4f5f7`, `--color-text-muted #9ba1aa`,
`--color-text-passive #646a73`; hairline white-alpha borders; accent
`--color-accent #4d8dff`. Status: working `#f59f4c`, needs-you `#e8c14a`,
mergeable `#74b98a`, fail `#ef6b6b`. Dark is primary (`:root`); light is the
supported override (`:root[data-theme="light"]`). The terminal keeps its own
palette.

### Cloned surfaces and approved divergences

- **Cloned surfaces:** the four-column gradient kanban board, the `ProjectSidebar`
  (brand + project disclosure + nested session rows + Settings menu footer), the
  session topbar (Kanban back button + identity + breathing `StatusBadge` pill), and
  the shared `DashboardTopbar`/`DashboardSubhead` chrome (Coding/Reviews tabs · "N
  working" pill · subhead) reused across board/review/PR/settings.
- **Approved divergence (2026-06-10):** on macOS, a titlebar cluster (sidebar toggle +
  back/forward history arrows, `TitlebarNav`) sits beside the traffic lights,
  VS Code-style — the web reference had no window chrome, so no analogue exists.
- **Approved divergence (2026-06-10):** the session inspector rail is fully
  collapsible, built on the shadcn resizable primitive (react-resizable-panels v4
  `collapsible` panel + imperative API,
  user-requested). The panel animates to 0% via a flex-grow transition while the
  content keeps a stable min-width (yyork-style, no mid-animation reflow). Toggled
  by a `PanelRight` icon button in the session topbar and ⌘⇧B; open state + split
  width persist. The AO reference keeps the rail always visible.
- **Approved divergence (2026-06-12):** the shell topbar spans the full window
  width and the sidebar is pinned below it (`top-14`), so the sidebar's right
  border stops at the header instead of cutting through the macOS traffic-light
  strip (user-requested). The AO reference keeps a full-height sidebar with the
  header beside it. On macOS the header always pads past the lights + TitlebarNav
  cluster (`.is-under-titlebar-nav`, 180px).

## Product Context

> "ReverbCode" below is a former name for this app; it survives in some URLs and
> identifiers. Read it as Agent Orchestrator.

- **What this is:** ReverbCode is an Electron desktop app for supervising many parallel
  AI coding-agent sessions, backed by a Go daemon (`backend/`). The `ao` CLI is the
  thin client over the same daemon.
- **Who it's for:** professional software engineers running multiple coding agents at
  once who need to delegate, watch, intervene, and ship PRs.
- **Space/peers:** agent orchestration / parallel-agent desktop tools. Closest peers:
  **emdash** (the primary design reference), **PostHog Code**, Conductor.
- **Project type:** dark-mode-primary desktop app; terminal-dense; keyboard-driven;
  runs all day.
- **The one memorable thing:** leverage and speed — "I'm more in control here than
  babysitting N terminal tabs myself."

### Product flow (what the UI must serve)

ReverbCode is **orchestrator-led**, which is the one thing that differs from emdash
(a flat list of independent sessions). Grounded in the daemon
(`backend/internal/session_manager/manager.go`, `docs/architecture.md`):

- A **Project** is a registered git repo.
- Per project there is **one active Orchestrator** session plus **N Worker** sessions.
  Both are the same underlying "session" (durable facts: `activity_state`,
  `is_terminated`, PR facts); they differ only by `Kind` (`KindOrchestrator` vs the
  default worker). A project may run the orchestrator on a different agent than its workers.
- The **Orchestrator is the human-facing coordinator**: you talk to it; it spawns
  workers (`ao spawn`), messages them (`ao send`), tracks progress, and synthesizes
  results. It avoids implementing unless necessary.
- A **Worker is a normal agent session** — nothing special-cased. It runs one focused
  task in a dedicated git worktree + branch, with the agent CLI in a terminal as the
  conversation, producing a diff → commit/push → PR. It escalates to the orchestrator
  only for true blockers or cross-session coordination.
- The daemon **observes** runtime + PR/CI/review facts and **derives** display status
  at read time: `working`, `needs_input`, `ci_failed`, `changes_requested`,
  `mergeable`, `approved`, `review_pending`, `pr_open`, `idle`, `terminated`, `merged`.
  Never store display status; keep session facts small.

## Aesthetic Direction

> **Superseded (2026-06-10):** see _Design direction — the shipped renderer is the
> reference_ at the top; authoritative values live in
> `frontend/src/styles/tokens.css`. The emdash framing below is retained for
> history and still reads true in spirit — the live look is the same flat
> near-black / hairline family.

- **Direction:** match **emdash** exactly — flat, near-black, hairline-bordered,
  utilitarian. Industrial control surface, calm chrome, the terminal as the center of gravity.
- **Decoration level:** minimal. Type + 1px hairlines do all the work. No gradients,
  glow, blobs, or emoji.
- **Mood:** low-glare, dense, keyboard-native; signal-over-noise.
- **Reference:** [emdash](https://github.com/generalaction/emdash) (primary, visual +
  structural), [PostHog Code](https://github.com/PostHog/code) (secondary). Tokens
  below were extracted from emdash's `src/renderer/index.css`.
- **Deliberate tradeoff:** to _be_ emdash, we use the **system font stack** (not a
  custom typeface) and emdash's neutral palette. We diverge in exactly one place: the
  accent is ReverbCode's **refined blue**, not emdash's jade green. The terminal keeps
  green (it is the agent CLI).

## Typography

System fonts only, like emdash — no custom/Google fonts, zero font payload.

- **UI / body / display:** `-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto,
Oxygen, Ubuntu, Cantarell, "Fira Sans", "Helvetica Neue", sans-serif` (San Francisco
  on macOS).
- **Mono / terminal / code / eyebrow labels:** `Menlo, Monaco, Consolas,
"Liberation Mono", "Courier New", monospace`.
- **Eyebrow labels** (section titles, dialog titles, the rail "PROJECTS" header):
  mono, **uppercase**, `letter-spacing: .12–.14em`, `--foreground-passive`.
- **Scale:** 14px base UI / sidebar (`text-sm`, weight 400) · 12px secondary + labels
  (`text-xs`) · 13px code/mono/terminal · 11px tiny · 10px micro + badges · 9px sidebar
  badge label. Buttons are `font-normal` (400), not bold.

## Color

emdash's flat Radix-neutral near-black ramp carries the whole interface; color is rare
and meaningful. Values are sRGB approximations of emdash's `color(display-p3 …)` tokens.

### Dark (primary)

| Role                                 | Hex             |
| ------------------------------------ | --------------- |
| `--bg` canvas                        | `#111111`       |
| `--bg-1` surface                     | `#191919`       |
| `--bg-2` raised / hover / active row | `#222222`       |
| `--bg-3`                             | `#2a2a2a`       |
| `--fg` text                          | `#eeeeee`       |
| `--fg-muted`                         | `#b4b4b4`       |
| `--fg-passive`                       | `#6e6e6e`       |
| `--border` hairline                  | `#3a3a3a`       |
| `--border-1`                         | `#484848`       |
| **`--accent` (blue)**                | **`#5b9dff`**   |
| `--needs-you` / in-progress (amber)  | `#ffcc4a`       |
| `--success` / mergeable (green)      | `#6cb16c`       |
| terminal green                       | `#7bd88f`       |
| `--error` (red)                      | `#d4544f`       |
| text selection                       | `#3f8ef7` @ 35% |
| terminal bg                          | `#161616`       |

### Light (supported, not primary)

| Role                      | Hex                               |
| ------------------------- | --------------------------------- |
| canvas / surface / raised | `#fcfcfc` / `#ffffff` / `#ededee` |
| text / muted / passive    | `#1a1a1a` / `#666666` / `#9a9a9a` |
| border                    | `#e3e3e5`                         |
| accent (blue)             | `#2563eb`                         |
| amber / green / red       | `#9a6b00` / `#1a7f37` / `#c0392b` |

### Accent rules

- **Blue** = the live edge only: primary buttons, the active/selected session, focus
  rings. Never decorative.
- **Amber** = an agent needs you (blocked / `needs_input` / `review_pending`).
- **Green** = `mergeable`/success and terminal/agent CLI text.
- **Red** = `ci_failed` / destructive.
- These map 1:1 to the daemon's derived statuses.

### Status indicator (no text badges)

Session status is a single ~14px glyph in one fixed slot, never a text pill/badge:

- **Working / active** → an animated spinner (accent).
- **Has an open PR** → a PR icon, tinted by PR state: mergeable/approved green,
  `ci_failed` red, review/`changes_requested` amber, plain `pr_open` muted.
- **Otherwise** → a filled dot: `needs_input` amber (pulsing), idle/done muted gray.

Precedence: **working spinner > PR icon > dot**. Implemented as `StatusGlyph` in
`components/SideRail.tsx`; used in the orchestrator's Workers list. (Worker rows in the
left rail stay name-only — no glyph.)

## Spacing

- **Base unit:** 4px (Tailwind scale: 1=4, 1.5=6, 2=8, 3=12, 4=16, 5=20, 6=24).
- **Density:** compact / desktop-tight.
- **Control + row height:** `h-8` = 32px default; `h-7` = 28px small; `h-6` = 24px xs.
- Inputs `px-2.5 py-1`; buttons `px-2.5`, gap 1–1.5.

## Layout

- **Approach:** fixed three-pane app shell, opens into the workbench (no marketing/dashboard home).
- **Panes:** `[ rail 240px ] [ center 1fr ] [ side rail 316px ]`.
- **Rail (240px), top → bottom:**
  1. **Orchestrator anchor** — pinned, single, visually distinct (blue 2px left bar,
     `--bg-2` fill, hub/`waypoints` icon, name "Orchestrator", a `5 agents · 2 need you`
     mono summary). This is ReverbCode's one addition over emdash. Default landing view.
  2. `PROJECTS` eyebrow label + a `+`.
  3. Project rows (folder icon + name) with nested **worker rows beneath**. Each project
     row has a hover-revealed **`+`** that opens the New-worker modal pre-scoped to that
     project (distinct from the `PROJECTS` header `+`, which registers a repo).
  4. **Footer:** `Search ⌘K`, `Settings ⌘,`. (No Library.)
  5. **Account** row pinned at the very bottom.
- **Worker rows are name-only.** Just the session name, truncated. Status, branch, diff,
  and PR live in the panes and topbar, never in the row. Selection = `--bg-2` fill + a
  2px blue left bar. (emdash itself shows a faint trailing timestamp; we omit it by choice.)
- **Center = the conversation.** Orchestrator → its coordination terminal (delegate here;
  composer reads "tell the orchestrator what to build"). Worker → the agent CLI terminal
  (tabbed per agent, e.g. `claude-code (1)`), with a composer (model selector, worktree
  path, `Accept edits`). The terminal **is** the conversation; no separate chat surface.
- **Side rail (316px):** orchestrator → a quiet **Workers** list (name + project + derived
  status). Worker → the **Git review rail**: `Changed N` → All files / Discard all / Stage
  all → file rows (`+adds −dels`, stage toggle) → `Commit message` + `Description` →
  **Commit & Push** (primary blue) → branch + `Create PR`.
- **Border radius:** `sm` 4px (scrollbar) · `md` 6px (buttons, inputs, toggles) ·
  `lg` 8px (rows, cards, panels) · `xl` 12px (modals) · `full` (badges/pills/dots).
- **Icons:** **lucide** only. No emoji.

### Topbar

- **Left (both):** `project / session` breadcrumb + pin; for the orchestrator, a hub icon
  - `Orchestrator`.
- **Right — worker session:** a **PR/CI status pill** that is the action
  (`PR #156 · mergeable` green / `CI failed` red / `review requested` amber /
  `Open PR` when none) → **Changes / Files / Terminal** view toggles → **⋯ session menu**
  (rename, restart, kill, claim PR — the `ao session …` commands).
- **Right — orchestrator:** **+ New worker** → Terminal toggle → **⋯ menu**. No diff toggles.

### Spawn-worker modal (mirrors emdash's Create Task)

You mostly let the orchestrator spawn workers from its conversation; the manual paths
(the topbar `+ New worker`, a project row's hover `+`, or `ao spawn`) open a modal that
mirrors emdash exactly. Launching from a project row pre-fills the Project field:

- Centered dialog, **12px radius**, `max-w` ~512px, `bg` canvas, `ring-1` at 10% fg,
  fade + zoom-95 enter.
- **Header:** eyebrow mono-uppercase title `New worker` + `×` close.
- **Body** (`gap` 15–16px): a **borderless large name field** (18px, auto-focus, slug
  rule "letters, numbers, hyphens") → **Project** selector → **Agent** selector
  (claude-code / codex / opencode / …) → a **"Based on"** bordered card with a segmented
  control `Branch · Issue · Pull Request` revealing a combobox → a **Prompt / Workspace**
  tab where Prompt is the worker's initial task (textarea).
- **Footer:** right-aligned single primary **`Spawn worker`** (blue) with a `⌘↵` keycap,
  disabled until valid.

## Motion

- **Approach:** minimal-functional. The one expressive exception: a status dot/spinner
  pulse on active/working sessions (opacity breathe) so "alive" is glanceable. Never
  animate text or layout.
- **Easing:** enter `ease-out`, exit `ease-in`, move `ease-in-out`.
- **Duration:** micro 80ms · short 160ms · medium 240ms · status pulse 1.8s loop ·
  modal enter ~150ms fade+zoom-95.

## Implementation notes

- The renderer (`frontend/src/renderer/styles.css`) currently uses **Inter** and a
  grayscale-blue theme. Migrate to this system: drop the Inter `font-family`, adopt the
  system stack, and replace the token values with the emdash neutral ramp + blue accent above.
- Keep tokens as CSS custom properties under `:root` (dark) and `:root[data-theme="light"]`.
- A faithful HTML reference of all of the above (both views + topbar + spawn modal,
  light/dark) is saved under
  `~/.gstack/projects/aoagents-agent-orchestrator/designs/design-system-20260609/`.

## Decisions Log

| Date       | Decision                                                               | Rationale                                                                                          |
| ---------- | ---------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| 2026-06-09 | Match emdash's visual language exactly                                 | User direction; emdash is the demonstrated reference for this app's UI.                            |
| 2026-06-09 | System font, not a custom typeface (e.g. Geist)                        | emdash uses the system stack; fidelity + native feel + zero font payload chosen over brand type.   |
| 2026-06-09 | Refined **blue** accent, not emdash's jade green                       | User's explicit pick; blue for primary/active/focus, terminal stays green.                         |
| 2026-06-09 | Single global **Orchestrator** anchor, orchestrator-first default view | The one real difference from emdash; orchestrator is the human-facing coordinator you delegate to. |
| 2026-06-09 | **Name-only** worker rows                                              | User direction; status/branch/diff live in panes + topbar, not the row.                            |
| 2026-06-09 | Removed **Library** from the rail footer                               | User direction; footer is Search + Settings only.                                                  |
| 2026-06-09 | Topbar right = PR/CI pill + view toggles + ⋯ menu (worker)             | Surfaces the actionable PR/CI state from the daemon; emdash/PostHog Code precedent.                |
| 2026-06-09 | Spawn modal mirrors emdash's Create Task                               | Consistency with the reference; mapped to `ao spawn` params.                                       |
