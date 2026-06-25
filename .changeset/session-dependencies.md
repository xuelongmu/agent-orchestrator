---
"@aoagents/ao-core": minor
---

feat(core): model session dependencies (dependsOn/blockedBy) and a blocked pre-state

Represent prerequisites on sessions so a scheduler can hold dependent work until
its prerequisites resolve:

- `Session`, `SessionSpawnConfig`, and `SessionMetadata` gain `dependsOn` and
  `blockedBy` (session and/or issue ids). They persist as comma-separated ids
  and survive restart.
- At spawn time, `dependsOn` is the union of the explicit config and the
  tracker's blocking relations (`Issue.blockedBy`, from #7); `blockedBy`
  defaults to the full set.
- A new `blocked_by_dependency` canonical session reason (on the existing
  `not_started` state) marks held sessions. `isBlockedByDependency()` is
  exported for consumers.
- When a session has unresolved prerequisites, `spawn()` records it as blocked
  and does **not** start work — no workspace, runtime, or agent launch. The
  lifecycle manager leaves the blocked pre-state untouched so it is never
  promoted to `working` until its prerequisites are cleared.
