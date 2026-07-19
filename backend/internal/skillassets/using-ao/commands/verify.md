# ao verify

Run one operator-approved verification profile outside the current agent's
terminal/ConPTY process tree. This command must run inside an AO session because
it resolves the workspace from `AO_SESSION_ID` and authenticates with the
session-scoped `AO_VERIFY_CAPABILITY` injected by AO.

This opaque value binds normal CLI requests and prevents blind or accidental
cross-session use. AO itself does not provide an OS-identity boundary between
same-user workers; hostile same-UID isolation is tracked in #150.

```text
ao verify <profile>
```

The daemon, not the CLI, resolves the profile to an argv. The API never accepts
an executable, shell text, or filter arguments. `backend` and `frontend` are
built-in profiles. Operators may configure narrower global or project-scoped
profiles in the absolute JSON file named by `AO_VERIFY_CONFIG_FILE` before
daemon startup. Worker-reachable project configuration cannot change this
policy.

The command waits, prints the outcome and absolute bounded-log path under
session-scoped `AO_DATA_DIR/verification/` storage, and exits nonzero for
failure, cancellation, or timeout. It does not write logs or ignore files into
the workspace. Read the printed log path for details:

```bash
ao verify backend-storage
cat <printed-log-path>
```
