# Local development

How to get a working Agent Orchestrator checkout on your machine and run AO from
source. If you only want to _use_ AO, install the desktop app instead — see
[README.md](../README.md#install). This page is for contributors.

## Prerequisites

| Tool               | Version                           | Why                                               |
| ------------------ | --------------------------------- | ------------------------------------------------- |
| Go                 | 1.25.x (`backend/go.mod`)         | backend, daemon, code generation                  |
| Node               | 20 baseline; 22/24 also work      | frontend, `npm run api:ts`                        |
| git                | 2.25+                             | session workspaces are git worktrees              |
| tmux (macOS/Linux) | 3.x                               | the terminal runtime the daemon execs per session |
| gh                 | any, authenticated                | pull request, CI, and review facts                |
| C toolchain        | Xcode CLI tools / build-essential | cgo and native npm modules                        |

On macOS and Linux, `tmux` is easy to miss and is not optional — the daemon
execs it for every session. **Windows does not need it**: the daemon uses the
built-in ConPTY runtime there instead. Either way `ao doctor` reports which
terminal runtime your platform uses and whether it is available.

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

### Nix

A dev shell is provided. `.envrc` contains `use flake`, so with
[direnv](https://direnv.net) installed the shell loads on `cd`:

```bash
nix develop      # or: direnv allow
```

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
cd frontend && npm run dev       # Electron app; spawns the daemon from source
cd frontend && npm run dev:web   # renderer only, in a normal browser
cd backend  && go run ./cmd/ao daemon   # daemon only, no UI
```

`ao daemon` is a hidden command — it is the daemon entrypoint the desktop app
spawns, not something end users invoke.

## Verify your setup

```bash
ao doctor
```

This is the fastest way to confirm an environment is good. It checks config, the
data directory, SQLite, the running daemon, git, your platform's terminal runtime
(tmux on macOS/Linux, ConPTY on Windows), whether the `ao` on your `PATH` is the
one you built, each agent harness it can find, and your gh token.

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

**Tests fail on paths like `/var/…` vs `/private/var/…`** — on macOS `TMPDIR`
lives under `/var`, a symlink to `/private/var`. Production code resolves
symlinks, so tests must resolve `t.TempDir()` through `filepath.EvalSymlinks`
before comparing paths.

**Sessions do not start when AO is launched from Finder or the Dock** — GUI apps
inherit launchd's minimal environment, not your shell's, so the daemon cannot
find `tmux`, `git`, or the agent CLIs. See
[daemon-environment.md](daemon-environment.md); launching from a terminal is the
quick workaround.
