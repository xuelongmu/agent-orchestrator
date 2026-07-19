# Worker/daemon threat model

Agent Orchestrator is a single-user, local supervisor. The daemon, desktop app,
CLI, and managed workers normally run as the same OS account. They are one
security principal even when AO gives sessions separate worktrees, processes,
and opaque identifiers.

This document describes the boundary AO provides today. It applies to Windows,
macOS, and Linux unless a platform is called out explicitly.

## Decision: hostile workers are out of scope

AO does **not** treat a managed worker as an adversary to the daemon or to
another same-user session. A malicious or compromised worker that can execute
arbitrary code as the AO user is outside the supported threat model. Worktrees,
session IDs, process trees, file modes, loopback listeners, and API
capabilities are not sandboxes or same-user security boundaries.

Verifier authorization therefore targets accidental and ambient isolation:
it binds a well-behaved `ao verify` invocation to the session and project that
received the capability, rejects missing or malformed capabilities, and stops
a caller that has only guessed another session ID. It does not claim to resist
a hostile same-user worker.

AO will not attempt to harden this boundary with another secret stored by, or
made available to, the same OS account. Supporting hostile workers would
require an OS-isolated design, such as a daemon or credential broker running as
a separate service identity plus workers confined by platform sandboxes and
ACLs. Such a design would also require authenticated IPC and cross-session
attack tests on every supported OS. That architecture is not part of the
current product scope.

## Assumed attacker access

A hostile same-user worker is assumed able to use any access the operating
system grants the shared account, including the following surfaces:

| Surface     | Windows                                                                                                                                                                                                                                 | macOS                                                                                                                                                                                     | Linux                                                                                                                                                                                               | AO consequence                                                                                                                                                                                                                       |
| ----------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Files       | Access is decided by the caller's access token and the file DACL, not ownership alone. AO processes sharing the user's token normally receive the same access; mandatory integrity or other policy can further restrict it.             | Owner-only modes such as `0600` still allow every process running as that owner.                                                                                                          | Owner-only modes such as `0600` still allow every process running as that UID.                                                                                                                      | The worker may read or alter `AO_DATA_DIR`, `running.json`, the SQLite database, verifier logs, the HMAC key, and other accessible session workspaces. File modes and ACLs protect against _other_ accounts, not the shared account. |
| Processes   | Inspection or termination requires process-handle rights granted by the caller's token and the target DACL. Windows has no generic POSIX signal right; integrity levels and protected-process policy can deny otherwise allowed access. | A matching UID normally permits POSIX signals. Entitlements, hardened-runtime protections, and system policy principally constrain debugging and inspection rather than ordinary signals. | Ordinary signal permission follows UID, capability, user-namespace, and LSM rules. Yama, proc mount policy, and dumpability constrain `ptrace` and `/proc` inspection rather than ordinary signals. | AO does not rely on workers being unable to inspect, interfere with, or terminate daemon and sibling-session processes. Process-tree ownership is for lifecycle cleanup, not adversarial containment.                                |
| Environment | A worker receives its own environment and same-account process inspection may expose other process state.                                                                                                                               | A worker receives its own environment; process inspection policy varies.                                                                                                                  | A worker receives its own environment and `/proc/<pid>/environ` may expose peer state when policy permits it.                                                                                       | Values injected into a session, including `AO_SESSION_ID` and `AO_VERIFY_CAPABILITY`, are not secrets from that worker. Daemon or sibling environments must not be treated as a dependable boundary either.                          |
| Loopback    | Any local process able to open the TCP port can call it.                                                                                                                                                                                | Any local process able to open the TCP port can call it.                                                                                                                                  | Any local process able to open the TCP port can call it.                                                                                                                                            | Binding the primary API to `127.0.0.1` prevents remote-network exposure; it does not authenticate a local process or its session. The primary listener intentionally remains unauthenticated.                                        |

Platform hardening may remove some individual techniques, but AO's guarantee
does not change. The shared account's file access alone is sufficient to defeat
a credential whose key is stored under `AO_DATA_DIR`, and loopback TCP does not
carry a portable, trustworthy peer identity.

## In-scope protections

AO does defend the following boundaries:

- The primary daemon listener is loopback-only, so remote hosts cannot connect
  to it directly. The separately documented, opt-in LAN listener has its own
  bearer-password boundary and route restrictions.
- Verification accepts an operator-configured profile name, never caller
  supplied argv, shell text, working directory, or environment overrides.
- A verification capability is scoped to a session/project tuple and compared
  in constant time. This catches missing, corrupted, guessed, or accidentally
  reused values when the caller has not read the shared key or another
  session's state.
- Verification process ownership and cancellation clean up supported process
  trees. Those guarantees do not prevent hostile same-user process
  interference. "Daemon-owned" describes lifecycle ownership only: verifier
  commands still run as the same OS account and are not a privileged broker.
- Worktrees reduce accidental source-tree conflicts. They do not provide
  confidentiality or integrity against another process running as the user.

## Operator guidance

Run only worker harnesses and verification tools that you trust with the AO
user account and all data that account can access. Treat every managed worker
as capable of calling the loopback API and reading AO state. If a workload is
not trusted, isolate the entire AO/worker deployment with an OS account, VM, or
container boundary appropriate to the host; AO does not create or validate
that boundary for you.

The verifier-specific policy, authorization flow, and process cleanup
guarantees are documented in [verification.md](verification.md).
