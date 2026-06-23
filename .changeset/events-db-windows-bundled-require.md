---
"@aoagents/ao-core": patch
---

fix(core): don't crash the dashboard on Windows when ao-core is bundled

`events-db.ts` called `createRequire(import.meta.url)` at module top-level. When
ao-core is inlined into a bundle (the Next.js dashboard server), the bundler
freezes `import.meta.url` to a stale build-machine path. On Windows that
POSIX-style `file://` URL is rejected by `createRequire` with
`ERR_INVALID_ARG_VALUE`, and because the call was at top level the throw happened
at import time — taking down every dashboard route that imports `@aoagents/ao-core`.

Move the `createRequire` into `openDb()` (already wrapped by `getDb()`'s
try/catch) with a cwd-anchored fallback base, so a mangled URL degrades to
"activity-events DB unavailable" (null) instead of crashing. Matches the existing
graceful-degradation contract that all `getDb()` callers already follow.
