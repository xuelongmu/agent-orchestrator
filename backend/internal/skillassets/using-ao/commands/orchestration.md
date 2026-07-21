# Project orchestration

AO gives each project one live orchestration policy:

- **Mission** is the default. The orchestrator works through its bounded task or issue set and receives no scheduled check-ins.
- **Charter** is a standing mandate. At the configured interval, AO checks whether exactly one project orchestrator is idle and, only then, asks it to refresh durable project/worker/tracker/PR/check/review state and continue genuinely actionable work.

Charter check-ins do not message active, waiting, blocked, rate-limited, exited, or terminated sessions. They do not create a second orchestrator, invent work, or duplicate active ownership. A daemon restart starts a fresh interval instead of immediately waking the orchestrator.

## Select the project

Pass the registered project id explicitly:

```bash
ao project orchestration get my-project
ao project orchestration set my-project --mode charter --interval 30m
```

Inside an AO session, prefer `--current`; it resolves the owning project from `AO_PROJECT_ID` or `AO_SESSION_ID`:

```bash
ao project orchestration set --current --mode charter --interval 30m
```

The command deliberately does not accept issue ids. Issues are missions within the selected project's broader charter.

## Change the live policy

```bash
# Bounded, one-shot coordination (the default for existing projects)
ao project orchestration set <project-id> --mode mission

# Standing coordination; intervals accept whole minutes from 1m through 24h
ao project orchestration set <project-id> --mode charter --interval 30m

# Temporarily suppress or restore scheduled charter check-ins
ao project orchestration pause <project-id>
ao project orchestration resume <project-id>
```

Policy updates are project-scoped and take effect without restarting AO. Pause/resume preserves the selected mode and interval and does not stop workers or ordinary lifecycle/SCM reactions. Add `--json` to any command for structured output.
