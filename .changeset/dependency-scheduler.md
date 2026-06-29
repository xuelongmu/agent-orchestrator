---
"@aoagents/ao-core": minor
---

feat(core): dependency-aware scheduler + spawn-session reaction (cross-repo ordering)

Automatically unblock and launch dependent sessions when their prerequisite work
merges — including across repos/projects, the keystone for "backend API merges →
start the frontend ticket":

- The lifecycle manager runs a dependency scheduler pass each poll over the full
  session set. When a prerequisite session's PR is merged, it narrows every held
  dependent's `blockedBy` (persisting immediately so progress survives the
  prerequisite's post-merge cleanup and an AO restart) and launches a dependent
  once all of its prerequisites are satisfied. Because the unscoped supervisor
  lists sessions across every project, a backend repo merge can unblock a
  frontend repo session.
- A dependent with multiple prerequisites stays blocked until all of them merge.
- Launches respect a new per-project `maxConcurrent` cap (orchestrators
  excluded); held sessions whose prerequisites are satisfied wait until the
  project is under the cap.
- `SessionManager` gains `unblock(sessionId)`, which launches a previously-held
  session reusing its reserved id and branch (so the branch still auto-links to
  the issue tracker). It is idempotent — a non-held record is returned unchanged.
- New `spawn-session` reaction action (on `ReactionConfig.action`) triggers a
  scheduler pass on demand.
