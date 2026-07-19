# Out-of-band workspace verification

`ao verify <profile>` lets a worker request a project-approved check without
starting that check in the worker's terminal/ConPTY process tree. The CLI sends
only `AO_SESSION_ID` and the profile name to the loopback daemon. The daemon
loads the session workspace and project configuration, resolves the profile to
an argv, and starts the executable directly; no shell evaluates request data.

Two current-architecture profiles are available by default:

- `backend`: `go test ./...` from `backend/`
- `frontend`: `npm test -- --run` from `frontend/`

Projects can add or replace profiles in their typed project config:

```json
{
  "verification": {
    "backend-storage": {
      "argv": ["go", "test", "./internal/storage/sqlite/..."],
      "workingDirectory": "backend",
      "timeoutSeconds": 300
    },
    "frontend-session": {
      "argv": ["npm", "test", "--", "--run", "src/session.test.ts"],
      "workingDirectory": "frontend"
    }
  }
}
```

Profile names are the allowlist. The API accepts no argv, shell text, working
directory, or environment overrides. Argument boundaries are preserved exactly.
Working directories are resolved through symlinks and must stay under the
session workspace.

## Outcomes and logs

The CLI waits for completion, prints `outcome` and the absolute `log` path, and
exits nonzero unless the outcome is `passed`. Outcomes are `passed`, `failed`,
`canceled`, and `timed_out`. A configured timeout is capped at one hour; zero
uses ten minutes.

Combined stdout/stderr is streamed to `.ao/verify-<n>.log` in the session
workspace. Each file retains at most 1 MiB (the newest output wins), `.ao` has a
local ignore rule, and the newest ten logs are retained. Run numbers are never
reused merely because old logs were pruned. AO refuses a `.ao` path that is a
symlink, Windows reparse point, or resolves outside the workspace, and creates
logs with exclusive create so an existing path cannot be overwritten.

Only one verification may run in a workspace. A concurrent request returns
`VERIFY_ALREADY_RUNNING`; different workspaces may verify concurrently.
Canceling the CLI request, reaching the profile timeout, or gracefully stopping
the daemon cancels the daemon-owned process tree. Unix runs use a separate
process group (and Linux parent-death signaling); Windows runs use a new process
group inside a kill-on-close Job Object, so they are not descendants of the
worker ConPTY and an abnormal daemon exit closes ownership. A restarted daemon
does not adopt an earlier run: it allocates the next log number and applies the
same retention cleanup. On macOS, an uncatchable daemon kill cannot provide a
kernel parent-death guarantee; ordinary request cancellation and daemon shutdown
still terminate the complete process group.

The route is `POST /api/v1/sessions/{sessionId}/verify`. It intentionally bypasses
the ordinary short REST timeout, enforces its own profile timeout, and is hidden
from the opt-in LAN listener even when Connect Mobile is enabled.
