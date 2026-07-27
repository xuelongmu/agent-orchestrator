package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// devVersion is the unstamped Version a plain `go build ./cmd/ao` produces.
// Release tooling replaces it via -ldflags (see version.go), so it is the marker
// that separates a contributor's own build from a distributed one.
const devVersion = "dev"

// backendModuleLine is the go.mod module declaration that identifies an AO
// checkout. Matching the module path rather than directory names keeps an
// unrelated repo that happens to have backend/ and frontend/ from qualifying.
const backendModuleLine = "module github.com/aoagents/agent-orchestrator/backend"

// startMode selects which app `ao start` launches.
type startMode int

const (
	// startModeAuto picks source when a dev build runs inside a checkout.
	startModeAuto startMode = iota
	startModeSource
	startModeRelease
)

// isDevBuild reports whether this binary was built from source without release
// stamping.
func isDevBuild() bool { return Version == devVersion }

// resolveStartMode turns the flag pair plus the ambient context into a concrete
// mode. Auto only chooses source for a dev-stamped binary inside a checkout: a
// distributed `ao` keeps launching the distributed app even when the user
// happens to be standing in a clone.
func resolveStartMode(requested startMode, checkout string, devBuild bool) startMode {
	if requested != startModeAuto {
		return requested
	}
	if checkout != "" && devBuild {
		return startModeSource
	}
	return startModeRelease
}

// findSourceCheckout walks dir and its ancestors for an AO source checkout,
// returning the repository root or "" when there is none.
func findSourceCheckout(dir string) string {
	for {
		if isSourceCheckout(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// isSourceCheckout reports whether dir is the root of an AO checkout: it must
// carry both halves of the repo, and backend/go.mod must declare AO's module.
func isSourceCheckout(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "frontend", "package.json")); err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, "backend", "go.mod"))
	if err != nil {
		return false
	}
	return bytes.Contains(data, []byte(backendModuleLine))
}

// runStartFromSource launches the desktop app from the checkout at root via the
// frontend dev harness, which spawns the daemon from this source tree.
//
// Unlike the release path this blocks. `npm run dev` is a dev server: its output
// and its Ctrl-C belong to the caller's terminal, and returning early would
// orphan it.
func (c *commandContext) runStartFromSource(ctx context.Context, out, errOut io.Writer, root string) error {
	frontend := filepath.Join(root, "frontend")

	npmPath, err := c.deps.LookPath("npm")
	if err != nil {
		return fmt.Errorf("ao start: running from source needs npm on PATH (install Node.js), or use `ao start --release`")
	}
	if _, err := os.Stat(filepath.Join(frontend, "node_modules")); err != nil {
		return fmt.Errorf("ao start: frontend dependencies are not installed; run `npm ci --prefix %s` first", frontend)
	}

	c.warnRunningDaemon(ctx, errOut)

	_, _ = fmt.Fprintf(out, "Starting Agent Orchestrator from source at %s\n", root)
	_, _ = fmt.Fprintf(out, "Running `npm run dev` in %s — press Ctrl-C to stop.\n", frontend)
	_, _ = fmt.Fprintf(out, "Use `ao start --release` to fetch and open the published desktop app instead.\n")

	name, args := devHarnessArgv(npmPath)
	return c.deps.RunAttached(ctx, frontend, name, args...)
}

// devRunFilePath resolves the run file the dev launch will actually consult,
// mirroring resolveDevDaemonConfig in frontend/src/shared/dev-daemon-config.ts:
// an explicit AO_RUN_FILE wins, otherwise ISOLATE_DEV=true moves state to
// ~/.ao/dev. Without this, an isolated launch would be judged against the
// canonical daemon it is specifically avoiding.
func devRunFilePath(canonical string) (path string, isolated bool) {
	isolated = os.Getenv("ISOLATE_DEV") == "true"
	if !isolated || strings.TrimSpace(os.Getenv("AO_RUN_FILE")) != "" {
		return canonical, isolated
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return canonical, isolated
	}
	return filepath.Join(home, ".ao", "dev", "running.json"), true
}

// devDaemonPort mirrors resolveDevDaemonConfig's port choice: an explicit
// AO_PORT (already resolved into cfg.Port) wins, otherwise isolation moves the
// daemon to 3002.
func devDaemonPort(configured int, isolated bool) int {
	if isolated && strings.TrimSpace(os.Getenv("AO_PORT")) == "" {
		return isolatedDevDaemonPort
	}
	return configured
}

// isolatedDevDaemonPort matches ISOLATED_DEV_DAEMON_PORT in
// frontend/src/shared/dev-daemon-config.ts.
const isolatedDevDaemonPort = 3002

// warnRunningDaemon prints a heads-up when a daemon the dev launch would attach
// to is already live.
//
// A dev launch does NOT enforce checkout identity by default: Electron passes
// enforceDevCheckout: devDaemonConfig.isIsolated, which is false unless
// ISOLATE_DEV=true. So the app attaches to whatever genuine AO daemon holds the
// port it is going to use rather than spawning one from this checkout — meaning
// backend changes here silently would not be running. Say that plainly; the
// failure is invisible otherwise.
func (c *commandContext) warnRunningDaemon(ctx context.Context, w io.Writer) {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	runFile, isolated := devRunFilePath(cfg.RunFilePath)
	port := devDaemonPort(cfg.Port, isolated)

	pid, live := 0, false
	if info, err := runfile.CheckStale(runFile); err == nil && info != nil {
		pid, port, live = info.PID, info.Port, true
	} else if probe, err := c.readProbe(ctx, port, "healthz"); err == nil && probe.Service == daemonmeta.ServiceName {
		// The run file is missing, stale, or unparseable, but a daemon may still
		// hold the port. Electron attaches in exactly that case via the
		// direct-port fallback in main.ts, so staying silent here would hide the
		// attachment the warning exists to surface.
		pid, live = probe.PID, true
	}
	if !live {
		return
	}
	remedy := "Stop it with `ao stop`, or set ISOLATE_DEV=true to use a separate data dir and port."
	if isolated {
		// ISOLATE_DEV is already on, so recommending it would be nonsense. Plain
		// `ao stop` is also wrong: it resolves through config.Load, which does
		// not read ISOLATE_DEV, so it would target the canonical daemon and
		// report success while the isolated one kept running.
		//
		// Name the two variables rather than emitting a command. A copy-paste
		// line would have to pick a shell — POSIX `VAR=x cmd` prefixes are not
		// valid in PowerShell or cmd.exe — and would need per-shell quoting for
		// paths containing spaces. docs/local-development.md carries the actual
		// invocations, where the shell is known.
		remedy = fmt.Sprintf("If it is not from this checkout, stop it by running `ao stop` with "+
			"AO_RUN_FILE=%q and AO_DATA_DIR=%q set (`ao stop` does not read ISOLATE_DEV); "+
			"see docs/local-development.md.",
			runFile, filepath.Join(filepath.Dir(runFile), "data"))
	}
	_, _ = fmt.Fprintf(w,
		"Warning: an AO daemon is already running (pid %d, port %d), and a dev launch\n"+
			"attaches to it instead of starting one from this checkout. If it is not this\n"+
			"checkout's daemon, your backend changes will not be running. %s\n",
		pid, port, remedy)
}
