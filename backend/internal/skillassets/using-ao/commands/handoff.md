# ao handoff

Seal the current agent session's immutable structured completion handoff. Call
it once when the task is completed or ready for review. An exact retry is safe;
a later payload with any different value or ordering is rejected. This command
does not terminate the session or change its activity state.

The command must run inside AO (`AO_SESSION_ID` must be set). The daemon bounds
the JSON payload to 256 KiB, changed files to 128 entries of at most 1024 UTF-8
bytes each, verification commands to 32 entries of at most 4096 UTF-8 bytes
each, and residual risk to 8192 UTF-8 bytes.

## Syntax

```text
ao handoff [flags]
```

## Flags

- `--changed-file <path>` — changed file path; repeat once per file.
- `--verification-command <command>` — command actually run; repeat as needed.
- `--residual-risk <text>` — remaining risk or deferred verification. Use an
  empty string when none remains.
- `-h / --help` — show authoritative CLI help.

Flag order and values are preserved as part of the exact immutable payload.

## Example

```bash
ao handoff \
  --changed-file backend/internal/domain/handoff.go \
  --changed-file frontend/src/renderer/components/SessionInspector.tsx \
  --verification-command "go test ./internal/domain" \
  --residual-risk "Full cross-platform verification is deferred to CI."
```
