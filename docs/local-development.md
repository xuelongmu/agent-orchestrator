# Local development

How to get a working Agent Orchestrator checkout on your machine and run AO from
source. If you only want to _use_ AO, install the desktop app instead — see
[README.md](../README.md#install). This page is for contributors.

## Prerequisites

| Tool               | Version                           | Why                                               |
| ------------------ | --------------------------------- | ------------------------------------------------- |
| Go                 | 1.25.7+ (`backend/go.mod`)        | backend, daemon, code generation                  |
| Node               | 20.19+ or 22.12+ (24 works)       | frontend, `npm run api:ts`                        |
| git                | 2.25+                             | session workspaces are git worktrees              |
| tmux (macOS/Linux) | 3.x                               | the terminal runtime the daemon execs per session |
| gh                 | any, authenticated                | pull request, CI, and review facts                |
| C toolchain        | Xcode CLI tools / build-essential | cgo and native npm modules                        |

On macOS and Linux, `tmux` is easy to miss and is not optional — the daemon
execs it for every session. **Windows does not need it**: the daemon uses the
built-in ConPTY runtime there instead. Either way `ao doctor` reports which
terminal runtime your platform uses and whether it is available.

The Node floor is not the major version alone: the locked Vite requires
`^20.19.0 || >=22.12.0`, so 20.0–20.18 and 22.0–22.11 are too old. `npm ci` only
warns about this, so the mismatch surfaces later as a confusing failure in
`npm run dev`.

### macOS

```bash
brew install go@1.25 node git tmux gh
```

`go@1.25` is **keg-only**: Homebrew installs it but does not link it onto your
`PATH`. Add it yourself, along with Go's bin directory so locally built tools
(including `ao`) resolve:

```bash
echo 'export PATH="$(brew --prefix go@1.25)/bin:$PATH"' >> ~/.zprofile
echo 'export PATH="$(go env GOPATH)/bin:$PATH"' >> ~/.zprofile
```

Let `brew --prefix` resolve the location rather than hardcoding it: keg-only
formulae live under `/opt/homebrew/opt` on Apple silicon but `/usr/local/opt` on
Intel. The lines are evaluated in order at shell startup, so the second one
resolves `go` via the first.

Open a new shell, then confirm with `go version`.

`brew install go` (no version suffix) currently gives 1.26.x. That builds the
module fine, but pinning 1.25 matches CI, which reads the version from
`backend/go.mod`.

The patch version matters: `go.mod` declares `go 1.25.7`, so 1.25.0–1.25.6 do
not satisfy it. With `GOTOOLCHAIN=local` those fail outright; otherwise Go tries
to download 1.25.7, which fails on an offline or restricted machine.

### Nix

A dev shell is provided. `.envrc` contains `use flake`, so with
[direnv](https://direnv.net) installed the shell loads on `cd`:

```bash
nix develop      # or: direnv allow
```

The shell provides Node 22 rather than CI's 20: Node 20 reached end-of-life in
April 2026, and nixpkgs refuses to build an end-of-life Node. 22 satisfies the
version floor above.

Nix is entirely optional — a Homebrew or distro toolchain works the same.

## Install dependencies

There are two npm installs, one at each of the repo root and `frontend/`:

```bash
npm ci
npm ci --prefix frontend
```

The root install provides `openapi-typescript` for `npm run api:ts`; the frontend
install provides the renderer and Electron toolchain.

Use `npm ci`, not `npm install`. `npm install` rewrites `package-lock.json` and
that drift shows up as unrelated noise in your next pull request.

## Run AO from source

Build the CLI and put it on your `PATH`:

```bash
cd backend && go install ./cmd/ao
```

Use `go install` rather than `go build -o …/bin/ao`: it puts the binary in
`$(go env GOPATH)/bin` and appends the right extension per platform. An explicit
`-o` output name produces an extensionless file on Windows, which `PATH`/`PATHEXT`
lookup will not find.

`go install` does not change your `PATH`, so that directory has to be on it or
the next step fails with command-not-found. The macOS snippet above already adds
it; on Linux add the equivalent to your shell profile, and on Windows add the
directory to your user `PATH`:

Append the line to the startup file your shell actually reads, then export it
once for the shell you are in. Most terminal emulators open an interactive
non-login shell, which reads `~/.bashrc` under bash and `~/.zshrc` under zsh —
`~/.profile` and `~/.zprofile` are only read by login shells, so a line placed
there will not survive opening a new terminal. (macOS Terminal.app is the
exception: it opens login shells, which is why the macOS snippet above uses
`~/.zprofile`.)

```bash
# Linux, bash
echo 'export PATH="$(go env GOPATH)/bin:$PATH"' >> ~/.bashrc
export PATH="$(go env GOPATH)/bin:$PATH"
```

```bash
# Linux, zsh
echo 'export PATH="$(go env GOPATH)/bin:$PATH"' >> ~/.zshrc
export PATH="$(go env GOPATH)/bin:$PATH"
```

```powershell
# Windows (PowerShell). The first line persists for future processes; the second
# updates the session you are in, which the first does not do.
[Environment]::SetEnvironmentVariable(
  "Path", "$([Environment]::GetEnvironmentVariable('Path','User'));$(go env GOPATH)\bin", "User")
$env:Path += ";$(go env GOPATH)\bin"
```

Check it resolves with `ao version` (a source build prints `dev`).

Alternatively, skip `PATH` entirely and invoke the binary by full path:

```bash
"$(go env GOPATH)/bin/ao" start       # bash/zsh
```

```powershell
& "$(go env GOPATH)\bin\ao.exe" start  # PowerShell needs the & call operator
```

Then, from anywhere inside the checkout:

```bash
ao start
```

A binary you built yourself (its version reports `dev`) run from inside a
checkout starts **that checkout** through the frontend dev harness, and blocks
until you stop it with Ctrl-C. A released `ao` — or `ao start` run from outside a
checkout — resolves the installed desktop app instead, downloading it if needed.

Force either path explicitly:

```bash
ao start --source     # always run this checkout
ao start --release    # always fetch and open the published desktop app
```

`ao start --source` is a wrapper over the underlying commands, which you can also
run directly:

```bash
cd frontend && npm run dev       # Electron app; starts the daemon from source
cd frontend && npm run dev:web   # renderer only, in a normal browser
cd backend  && go run ./cmd/ao daemon   # daemon only, no UI
```

`ao daemon` is a hidden command — it is the daemon entrypoint the desktop app
starts (through an Aqua LaunchAgent on macOS), not something end users invoke.

## Verify your setup

```bash
ao doctor
```

This is the fastest way to confirm an environment is good. It checks config, the
data directory, SQLite, the running daemon, git, your platform's terminal runtime
(tmux on macOS/Linux, ConPTY on Windows), whether the `ao` on your `PATH` is the
one you built, each agent harness it can find, and your gh token. On macOS it
also asks the running daemon to perform a login-keychain interaction from its
own process context; `keychain-session: FAIL` means daemon-spawned workers
cannot use login-keychain credentials.

It does **not** check Node or either `node_modules`, so a green `ao doctor` on a
checkout that skipped the `npm ci` steps above will still fail at
`ao start --source`. The preflight in `ao start --source` covers that separately.

Check where state lives with `ao status`. Everything must resolve under `~/.ao`
(overridable with `AO_DATA_DIR` / `AO_RUN_FILE`) — never
`~/Library/Application Support` or another OS default. See the hard rule in
[AGENTS.md](../AGENTS.md).

## Everyday commands

From the repo root:

```bash
npm run lint                 # backend go test ./... + golangci-lint v2.12.2
npm run frontend:typecheck   # frontend TypeScript check
npm run api                  # regenerate the OpenAPI spec + frontend TS types
npm run sqlc                 # regenerate sqlc output from queries/schema
```

Backend:

```bash
cd backend
go build ./...
go test ./...
go test -race ./...
go vet ./...
gofmt -l .          # must print nothing
```

Frontend:

```bash
cd frontend
npm run typecheck
npx vitest run
```

Packaging the desktop app is a separate, slower step. Both commands run
`build:daemon` first, so they need the Go toolchain as well:

```bash
cd frontend
npm run package   # build the app bundle
npm run make      # build distributables
```

Optional, and requiring Docker (not installed by default):

```bash
npx @redwoodjs/agent-ci run --all             # local workflow validation
docker build -f test/cli/Dockerfile -t ao-cli-smoke . && docker run --rm --init ao-cli-smoke
```

## Troubleshooting

**macOS workers hang with keychain-backed credentials** — the desktop app starts
the daemon through a LaunchAgent in the user's `gui/<uid>` launchd domain. It
restarts after crashes but stays stopped after a graceful `ao stop`. Its
generated plist and label-specific logs live beside the selected run file under
`~/.ao/launchd` and `~/.ao` (or the corresponding `AO_RUN_FILE` override).
This keeps the daemon, tmux server, and worker panes in the Aqua audit session
where login-keychain interaction is allowed. Run `ao doctor` and check
`keychain-session`; if it fails, stop and start the daemon from the desktop app
to unload any stale job and bootstrap the current definition.

**Processes still around right after Ctrl-C** — shutdown is graceful, not
immediate. The daemon drains on SIGTERM within `AO_SHUTDOWN_TIMEOUT` (default
10s), so a check run a second later will still see it holding the port and its
run file. Wait for that window before concluding something leaked.

Two things legitimately survive and are not leaks: `ao pty-host` processes
belonging to existing sessions, which are designed to outlive a daemon restart,
and anything owned by a _different_ AO install. When testing shutdown, use
isolated state so the dev app cannot restore real sessions and confuse the
result:

```bash
ISOLATE_DEV=true ao start --source
```

**Your backend changes do not seem to be running** — a dev launch attaches to
whatever AO daemon already holds the canonical port rather than starting one
from your checkout. Checkout identity is only enforced when `ISOLATE_DEV=true`,
so an installed `/Applications/Agent Orchestrator.app` or a stray `npm run dev`
will be used silently. `ao start --source` warns when it sees a live daemon.
Either stop the other one:

```bash
ao stop
```

or run isolated, which uses its own data dir (`~/.ao/dev`) and port (3002) so
both can coexist:

```bash
ISOLATE_DEV=true ao start --source
```

Note that `ao stop` does not read `ISOLATE_DEV` — it resolves through the
canonical config, so it would stop the wrong daemon and report success. To stop
an isolated one, point it at the isolated paths explicitly:

```bash
AO_RUN_FILE="$HOME/.ao/dev/running.json" AO_DATA_DIR="$HOME/.ao/dev/data" ao stop
```

```powershell
$env:AO_RUN_FILE = "$HOME\.ao\dev\running.json"
$env:AO_DATA_DIR = "$HOME\.ao\dev\data"
ao stop
```

PowerShell and `cmd.exe` do not accept the POSIX `VAR=value command` prefix form,
which is why the two are spelled out separately.

**Tests fail on paths like `/var/…` vs `/private/var/…`** — on macOS `TMPDIR`
lives under `/var`, a symlink to `/private/var`. Production code resolves
symlinks, so tests must resolve `t.TempDir()` through `filepath.EvalSymlinks`
before comparing paths.

**Sessions do not start when AO is launched from Finder or the Dock** — GUI apps
inherit launchd's minimal environment, not your shell's, so the daemon cannot
find `tmux`, `git`, or the agent CLIs. See
[daemon-environment.md](daemon-environment.md); launching from a terminal is the
quick workaround.
