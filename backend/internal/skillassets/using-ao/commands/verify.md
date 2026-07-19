# ao verify

Run one project-approved verification profile outside the current agent's
terminal/ConPTY process tree. This command must run inside an AO session because
it resolves the workspace from `AO_SESSION_ID`.

```text
ao verify <profile>
```

The daemon, not the CLI, resolves the profile to an argv. The API never accepts
an executable or shell text. `backend` and `frontend` are built-in profiles;
projects may configure narrower profiles such as `backend-storage`.

The command waits, prints the outcome and absolute `.ao/verify-<n>.log` path,
and exits nonzero for failure, cancellation, or timeout. Read that bounded log
for details:

```bash
ao verify backend-storage
cat <printed-log-path>
```
