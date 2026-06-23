/**
 * SQLite database layer for activity event logging.
 *
 * Lazy-initialized singleton. Opens on first call to getDb(), never on import.
 * Returns null if better-sqlite3 is unavailable (native build failure, optional dep).
 * WAL mode + busy_timeout for multi-process concurrent access.
 */

import { createRequire } from "node:module";
import { mkdirSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { getAoBaseDir } from "./paths.js";

type BetterSqlite3Database = {
  pragma(source: string, options?: { simple?: boolean }): unknown;
  exec(source: string): void;
  prepare(source: string): { run(...args: unknown[]): unknown; all(...args: unknown[]): unknown[] };
  close(): void;
};

let _db: BetterSqlite3Database | null = null;
let _dbFailed = false;
let _ftsEnabled = false;
let _dbUnavailableWarningEmitted = false;
const PRUNE_BATCH_SIZE = 1000;

function getEventsDbPath(): string {
  return join(getAoBaseDir(), "activity-events.db");
}

function initSchema(db: BetterSqlite3Database): void {
  db.exec(`
    CREATE TABLE IF NOT EXISTS activity_events (
      id         INTEGER PRIMARY KEY AUTOINCREMENT,
      ts_epoch   INTEGER NOT NULL,
      ts         TEXT NOT NULL,
      project_id TEXT,
      session_id TEXT,
      source     TEXT NOT NULL,
      type       TEXT NOT NULL,
      log_level  TEXT NOT NULL DEFAULT 'info',
      summary    TEXT NOT NULL,
      data       TEXT
    );

    CREATE INDEX IF NOT EXISTS idx_ae_ts      ON activity_events(ts_epoch);
    CREATE INDEX IF NOT EXISTS idx_ae_session ON activity_events(session_id);
    CREATE INDEX IF NOT EXISTS idx_ae_project ON activity_events(project_id);
    CREATE INDEX IF NOT EXISTS idx_ae_type    ON activity_events(type);
    CREATE INDEX IF NOT EXISTS idx_ae_source  ON activity_events(source);
  `);
}

function initFts(db: BetterSqlite3Database): void {
  db.exec(`
    CREATE VIRTUAL TABLE IF NOT EXISTS activity_events_fts USING fts5(
      summary, data,
      content='activity_events',
      content_rowid='id'
    );

    CREATE TRIGGER IF NOT EXISTS activity_events_ai
      AFTER INSERT ON activity_events
    BEGIN
      INSERT INTO activity_events_fts(rowid, summary, data)
        VALUES (new.id, new.summary, new.data);
    END;

    CREATE TRIGGER IF NOT EXISTS activity_events_ad
      AFTER DELETE ON activity_events
    BEGIN
      INSERT INTO activity_events_fts(activity_events_fts, rowid, summary, data)
        VALUES ('delete', old.id, old.summary, old.data);
    END;

    CREATE TRIGGER IF NOT EXISTS activity_events_au
      AFTER UPDATE ON activity_events
    BEGIN
      INSERT INTO activity_events_fts(activity_events_fts, rowid, summary, data)
        VALUES ('delete', old.id, old.summary, old.data);
      INSERT INTO activity_events_fts(rowid, summary, data)
        VALUES (new.id, new.summary, new.data);
    END;
  `);

  // Backfill rows written before FTS was available; triggers only cover future writes.
  db.exec("INSERT INTO activity_events_fts(activity_events_fts) VALUES('rebuild')");
}

function pruneOldEvents(db: BetterSqlite3Database, cutoff: number): void {
  db.prepare(
    `DELETE FROM activity_events
       WHERE rowid IN (
         SELECT rowid FROM activity_events WHERE ts_epoch < ? LIMIT ?
       )`,
  ).run(cutoff, PRUNE_BATCH_SIZE);
}

// Resolve the native better-sqlite3 binding via createRequire.
//
// `import.meta.url` is the natural base, but bundlers inline this module into a
// larger output (the dashboard bundles ao-core) and freeze import.meta.url to a
// stale build-machine path. On Windows that POSIX-style file URL is rejected by
// createRequire with ERR_INVALID_ARG_VALUE; when this ran at module top-level it
// threw at import time and took down every dashboard route that imports ao-core.
// Doing it here (inside openDb, which getDb wraps in try/catch) plus a cwd-anchored
// fallback base means a mangled URL degrades to "DB unavailable" (null) instead.
function requireBetterSqlite3(): new (path: string) => BetterSqlite3Database {
  let req: ReturnType<typeof createRequire>;
  try {
    req = createRequire(import.meta.url);
  } catch {
    req = createRequire(pathToFileURL(join(process.cwd(), "noop.js")).href);
  }
  return req("better-sqlite3") as new (path: string) => BetterSqlite3Database;
}

function openDb(): BetterSqlite3Database {
  const Database = requireBetterSqlite3();
  mkdirSync(getAoBaseDir(), { recursive: true });
  const db = new Database(getEventsDbPath());

  db.pragma("journal_mode = WAL");
  db.pragma("busy_timeout = 3000");
  // WAL + NORMAL gives one checkpoint window of exposure (acceptable for a diagnostic log)
  db.pragma("synchronous = NORMAL");

  const version = db.pragma("user_version", { simple: true }) as number;
  initSchema(db);
  if (version < 1) {
    db.pragma("user_version = 1");
  }

  try {
    initFts(db);
    _ftsEnabled = true;
  } catch (err) {
    _ftsEnabled = false;
    // eslint-disable-next-line no-console
    console.warn(
      "[ao] activity-events FTS unavailable — writes will continue and search will use a bounded LIKE fallback:",
      err instanceof Error ? err.message : String(err),
    );
  }

  // 7-day retention using epoch comparison (no text/datetime ambiguity)
  const cutoff = Date.now() - 7 * 24 * 60 * 60 * 1000;
  pruneOldEvents(db, cutoff);

  return db;
}

/** Whether the current process has an initialized FTS5 search table. */
export function isActivityEventsFtsEnabled(): boolean {
  return _ftsEnabled;
}

/**
 * Close the cached DB connection and reset module state.
 *
 * On Windows the SQLite handle holds an exclusive file lock; without an
 * explicit close, callers cannot remove `activity-events.db` (or its parent
 * directory) until the process exits. Tests that recreate the AO base dir
 * across runs must call this before `rmSync`.
 */
export function closeDb(): void {
  if (_db) {
    try {
      _db.close();
    } catch {
      // best-effort: connection may already be closed
    }
    _db = null;
  }
  _dbFailed = false;
  _ftsEnabled = false;
}

function isAoEventsInvocation(argv = process.argv): boolean {
  return argv.slice(2).includes("events");
}

function isMissingBetterSqlite3Binding(err: unknown): boolean {
  const message = err instanceof Error ? err.message : String(err);
  return (
    message.includes("Could not locate the bindings file") ||
    message.includes("better_sqlite3.node") ||
    message.includes("Cannot find module 'better-sqlite3'")
  );
}

function firstErrorLine(err: unknown): string {
  return (err instanceof Error ? err.message : String(err)).split(/\r?\n/, 1)[0] ?? "unknown error";
}

export function formatActivityEventsDbUnavailableWarning(err: unknown): string {
  if (isMissingBetterSqlite3Binding(err)) {
    return `[ao] activity-events disabled: better-sqlite3 not compiled for Node ${process.version} (ABI v${process.versions.modules}). Run \`pnpm rebuild better-sqlite3\` or use a supported Node version.`;
  }
  return `[ao] activity-events disabled: better-sqlite3 failed to load: ${firstErrorLine(err)}`;
}

export function emitActivityEventsDbUnavailableWarning(err: unknown): void {
  if (_dbUnavailableWarningEmitted) return;
  if (process.env["AO_DEBUG"] !== "1" && !isAoEventsInvocation()) return;
  _dbUnavailableWarningEmitted = true;
  // eslint-disable-next-line no-console
  console.warn(formatActivityEventsDbUnavailableWarning(err));
}

export function __resetActivityEventsDbWarningForTests(): void {
  _dbUnavailableWarningEmitted = false;
}

/**
 * Get the lazily-initialized DB connection.
 * Returns null if better-sqlite3 failed to load or init — callers should treat null as no-op.
 */
export function getDb(): BetterSqlite3Database | null {
  if (_dbFailed) return null;
  if (_db) return _db;
  try {
    _db = openDb();
    return _db;
  } catch (err) {
    _dbFailed = true;
    emitActivityEventsDbUnavailableWarning(err);
    return null;
  }
}
