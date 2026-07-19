# Out-of-band workspace verification

`ao verify <profile>` lets a worker request an operator-approved check without
starting that check in the worker's terminal/ConPTY process tree. The CLI
identifies the calling session with `AO_SESSION_ID` and sends the profile name
to the loopback daemon. The daemon resolves the session workspace and the
profile's preconfigured argv, then starts the executable directly; no shell
evaluates request data.

Two current-architecture profiles are available by default:

- `backend`: `go test ./...` from `backend/`
- `frontend`: `npm test -- --run` from `frontend/`

## Startup policy

Additional global or project-scoped profiles come only from an operator-owned
startup policy. Set `AO_VERIFY_CONFIG_FILE` to the absolute path of a JSON file
before starting the daemon:

```json
{
	"profiles": {
		"backend-storage": {
			"argv": ["go", "test", "./internal/storage/sqlite/..."],
			"workingDirectory": "backend",
			"timeoutSeconds": 300
		}
	},
	"projects": {
		"my-project-id": {
			"frontend-session": {
				"argv": ["npm", "test", "--", "--run", "src/session.test.ts"],
				"workingDirectory": "frontend"
			}
		}
	}
}
```

The daemon loads and validates this file once, before serving requests. An
empty `AO_VERIFY_CONFIG_FILE` enables only the compiled profiles. Invalid
profiles fail daemon startup. Project configuration APIs deliberately cannot
read or change this policy, so a worker cannot turn project-config write access
into arbitrary process execution.

Profile names are the request allowlist. The verification API accepts no argv,
shell text, working directory, environment override, or caller-supplied filter.
Configured commands preserve argument boundaries and known shell executables
are rejected. Working directories are resolved through symlinks and must stay
under the owning session's workspace.

## Session authorization

AO injects an opaque `AO_VERIFY_CAPABILITY` into each managed session. `ao
verify` sends it as the required `X-AO-Verification-Capability` request header.
The daemon verifies the capability against both the requested session and its
project before returning session-, workspace-, or project-specific state. A
capability from one session therefore cannot request verification for another
session, even when its identifier is known. Do not copy capabilities between
sessions or put them in logs.

The route is `POST /api/v1/sessions/{sessionId}/verify`. It intentionally
bypasses the ordinary short REST timeout, enforces its own profile timeout, and
is unavailable on the opt-in LAN listener even when Connect Mobile is enabled.

## Outcomes and logs

The CLI waits for completion, prints `outcome` and the absolute `log` path, and
exits nonzero unless the outcome is `passed`. Outcomes are `passed`, `failed`,
`canceled`, and `timed_out`. A configured timeout is capped at one hour; zero
uses ten minutes.

Combined stdout/stderr is streamed to session-scoped storage under
`AO_DATA_DIR/verification/session-<session-id-hash>/`, not into the project or session
workspace. Each log retains at most 1 MiB (the newest output wins), and the
newest ten logs are retained. Run numbers are not reused merely because old
logs were pruned. The CLI returns the safely created absolute path so the
owning worker can read the bounded log. Verification does not create a
workspace `.ao` directory or change a project's `.gitignore`.

Only one verification may run in a workspace. A concurrent request returns
`VERIFY_ALREADY_RUNNING`; different workspaces may verify concurrently.
Canceling the CLI request, reaching the profile timeout, or gracefully stopping
the daemon cancels the daemon-owned process tree. Unix uses an outer guardian
and a separate process group; Windows creates the guardian suspended, assigns
it to a kill-on-close Job Object, and only then resumes it. The verifier is
therefore outside the worker ConPTY process tree and cannot run before Windows
containment is established. A restarted daemon does not adopt an earlier run:
it allocates the next log number and applies the same retention cleanup.

Unix process groups contain ordinary child and grandchild processes, including
when the daemon exits unexpectedly. A deliberately hostile verifier descendant
can call `setsid(2)` to leave that process group; portable Darwin job-style
containment is not available, so operator policy must approve only trusted
verification tools. Windows Job Object containment does not have this gap.
