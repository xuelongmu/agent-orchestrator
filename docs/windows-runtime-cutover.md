# Windows runtime cutover

AO's supported Windows installation is the desktop release. The desktop
executable is normally installed at:

```text
%LOCALAPPDATA%\Programs\Agent Orchestrator\agent-orchestrator.exe
```

The desktop starts the migrated Go daemon/CLI at:

```text
%LOCALAPPDATA%\Programs\Agent Orchestrator\resources\daemon\ao.exe
```

A per-machine install uses the equivalent paths below
`%ProgramFiles%\Agent Orchestrator`. The bundled `resources\daemon\ao.exe` is
the **canonical migrated command entry point**. For source dogfood, use an
explicit `ao.exe` built from the current checkout instead. Do not use a bare
`ao` until its resolution has been verified.

The npm `@aoagents/ao` package can be a migrated bootstrap that launches its
matching platform Go binary. The older `@aoagents/ao-cli` package, and older
`@aoagents/ao` wrappers that depend on it, are the legacy TypeScript runtime
regardless of their version string. They use a different command surface and
state root.

## Read-only inventory

Run this before changing PATH, fnm, npm packages, state, or processes:

```powershell
# PowerShell resolution order, including duplicate fnm multishell entries.
Get-Command -All ao |
  Select-Object CommandType, Name, Source, Path, Version

# Every ao-shaped file currently reachable through PATH.
$env:PATH -split ';' |
  Where-Object { $_ } |
  ForEach-Object {
    Get-ChildItem -LiteralPath $_ -ErrorAction SilentlyContinue |
      Where-Object { $_.Name -in @('ao', 'ao.exe', 'ao.cmd', 'ao.ps1') }
  } |
  Select-Object -Unique FullName, Length, LastWriteTime

$desktop = Join-Path $env:LOCALAPPDATA `
  'Programs\Agent Orchestrator\agent-orchestrator.exe'
$canonical = Join-Path $env:LOCALAPPDATA `
  'Programs\Agent Orchestrator\resources\daemon\ao.exe'
Get-Item -LiteralPath $desktop, $canonical -ErrorAction SilentlyContinue

# Invoke the chosen binary by absolute path. A bare `ao --version` tests the
# shadow, not necessarily the migrated installation.
& $canonical --version

# Inventory roots without reading credentials or modifying either tree.
Get-Item "$HOME\.ao", "$HOME\.agent-orchestrator" `
  -ErrorAction SilentlyContinue
Get-ChildItem "$HOME\.ao", "$HOME\.agent-orchestrator" `
  -Force -ErrorAction SilentlyContinue |
  Select-Object FullName, LastWriteTime
Get-Item Env:AO_DATA_DIR, Env:AO_RUN_FILE -ErrorAction SilentlyContinue

# Process inventory only. Do not pipe these results to Stop-Process.
Get-CimInstance Win32_Process |
  Where-Object {
    $_.ExecutablePath -match 'Agent Orchestrator|agent-orchestrator|ao\.exe' -or
    $_.CommandLine -match 'agent-orchestrator|ao\.exe'
  } |
  Select-Object ProcessId, Name, ExecutablePath, CommandLine
```

`Get-Command -All ao` may show `ao.ps1`, `ao.cmd`, and extensionless wrappers
for each active fnm multishell. They can all lead to one legacy npm install, so
the number of results is not the number of independent AO installations.

## Detection and verification

Run doctor through the absolute migrated binary:

```powershell
& $canonical doctor
```

On Windows, doctor inspects the first `ao` found by executable PATH lookup. For
an fnm/npm `.cmd` shim, it accepts only a narrow no-shell shape and reads the
sibling `node.exe` and package entry as regular files, then reads the associated
`package.json` name/bin mapping and version. It never runs the shadowing shim,
Node, PowerShell, or `cmd.exe`.
Malformed/untrusted shims, recognized migrated npm bootstraps, and different
executables remain warnings; a trusted `@aoagents/ao-cli` package or an
`@aoagents/ao` wrapper that depends on it is a failure. Direct UNC and Windows
device namespace entries are rejected before their targets are inspected;
local drive paths can still traverse mapped drives or reparse points. The
failure reports both the shadowing path and the canonical running binary.
Doctor does not edit PATH or shims and does not stop, clean, migrate, or archive anything. (After this
preflight passes, doctor's pre-existing data-directory writability check
creates and removes one temporary probe file under the configured Go data
directory.)

Check `AO_DATA_DIR` and `AO_RUN_FILE` before running doctor. Do not run the Go
doctor with either variable pointing into `~/.agent-orchestrator`; clear or
override an inherited legacy value only in a deliberately isolated process.

The migrated installation is internally consistent when all of these are true:

- `Get-Command ao` resolves to the intended migrated command or bootstrap;
- `ao --version` and the absolute canonical binary report migrated versions;
- `~/.ao/running.json` and `~/.ao/data` are the active handshake/state paths,
  unless `AO_RUN_FILE` or `AO_DATA_DIR` deliberately overrides them;
- daemon health reports port 3001 and its executable path is the canonical Go
  binary;
- manual review uses the desktop inspector's **Run review** action (or the
  migrated review API), not legacy `ao review run --execute`.

Go-daemon-spawned worker and reviewer sessions prepend the daemon executable's
directory to their PATH. That prevents a legacy shim from taking over inside
new migrated sessions. It does not repair interactive shells or sessions that
were spawned by the legacy daemon.

## Safe cutover (approval required)

The runtime cutover is deliberately not automatic. Do not perform any step in
this section while a legacy AO process or session still depends on the old
install. Never stop, kill, clean, remove, or move a live AO process, session,
worktree, or state root without explicit per-instance approval.

1. Record the read-only inventory above and let legacy work finish normally.
   Get approval for every remaining process/session before stopping it.
2. Preserve `~/.agent-orchestrator` while the legacy runtime is retained. Do
   not point `AO_DATA_DIR` at it: the Go database belongs under `~/.ao` and the
   two state formats are not interchangeable.
3. After approved shutdown of all relevant writers, preview supported project
   import with the absolute Go binary:

   ```powershell
   & $canonical import --from "$HOME\.agent-orchestrator" --dry-run
   ```

   `ao import` reads legacy files without modifying them and imports only
   supported project/config data. A real import requires the migrated daemon
   to be stopped and takes its exclusive database-writer lease. Session/run
   history is not imported; retain or archive the legacy root if it is needed.

4. With separate approval, remove the legacy npm package from the relevant
   fnm Node installation or place the intended migrated command ahead of it in
   the persistent user PATH. Do not edit ephemeral `fnm_multishells` wrappers
   individually; fnm recreates them in new shells.
5. Open a new PowerShell and repeat the inventory and verification checks.
   Existing shells keep their old PATH and command cache.

Until those steps are approved and completed, the legacy runtime is temporary,
unsupported, and retained only for already-running work. Invoke each runtime by
absolute path to prevent cross-use.

## Rollback

Before cutover, record the original user PATH, fnm default Node version, npm AO
version, and archive location. If the new-shell verification fails:

1. restore the recorded PATH/fnm selection or reinstall the recorded npm
   version;
2. keep using absolute paths so legacy and migrated state cannot be confused;
3. restore an archived legacy root to its original location only after
   confirming no process is writing either copy; and
4. rerun the read-only inventory before launching anything.

Rollback never copies `~/.agent-orchestrator` over `~/.ao`, and it never starts,
stops, or cleans sessions as a side effect.
