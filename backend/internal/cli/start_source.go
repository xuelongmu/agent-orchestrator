package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
	// startModeAuto protects development contexts from implicitly installing a
	// release: a checkout runs source, and a dev build outside one stops with
	// guidance to change directory or opt in with --release.
	startModeAuto startMode = iota
	startModeSource
	startModeRelease
	startModeDevOutsideCheckout
)

// isDevBuild reports whether this binary was built from source without release
// stamping.
func isDevBuild() bool { return Version == devVersion }

// resolveStartMode turns the flag pair plus the ambient context into a concrete
// mode. A checkout is the strongest development signal, regardless of which
// `ao` happens to be first on PATH. A dev-stamped binary outside a checkout is
// also protected from silently fetching a release; runStart explains how to
// choose a source checkout or explicitly opt into --release.
func resolveStartMode(requested startMode, checkout string, devBuild bool) startMode {
	if requested != startModeAuto {
		return requested
	}
	if checkout != "" {
		return startModeSource
	}
	if devBuild {
		return startModeDevOutsideCheckout
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

	if strings.TrimSpace(os.Getenv("AO_DAEMON_COMMAND")) == "" {
		if err := c.checkRunningDevDaemon(ctx, errOut, root); err != nil {
			return err
		}
	} else {
		_, _ = fmt.Fprintln(errOut, "AO_DAEMON_COMMAND is set; using the explicitly configured daemon.")
	}

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

// checkRunningDevDaemon validates a daemon the dev launch would attach to. It
// fails closed unless the daemon proves it came from this checkout; Electron
// repeats the same check after npm starts to cover races with this preflight.
func (c *commandContext) checkRunningDevDaemon(ctx context.Context, w io.Writer, root string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("ao start: load daemon configuration: %w", err)
	}
	runFile, isolated := devRunFilePath(cfg.RunFilePath)
	configuredPort := devDaemonPort(cfg.Port, isolated)
	port := configuredPort

	pid, runFileLive := 0, false
	if info, err := runfile.CheckStale(runFile); err == nil && info != nil {
		pid, port, runFileLive = info.PID, info.Port, true
	}

	probe, probeErr := c.readProbe(ctx, port, "healthz")
	if runFileLive && (probeErr != nil || probe.Service != daemonmeta.ServiceName || probe.PID != pid) {
		// Electron treats a missing, stale, or PID-inconsistent run file as a
		// hint failure and probes the configured port directly. Mirror that
		// fallback before rejecting a genuine same-checkout daemon.
		if fallback, err := c.readProbe(ctx, configuredPort, "healthz"); err == nil && fallback.Service == daemonmeta.ServiceName {
			probe, probeErr, port, runFileLive = fallback, nil, configuredPort, false
		}
	}
	if probeErr != nil || probe.Service != daemonmeta.ServiceName {
		if !runFileLive {
			return nil
		}
		return fmt.Errorf("ao start: an AO daemon appears to be running (pid %d, port %d), but its checkout identity could not be verified; %s", pid, port, devDaemonStopRemedy(runFile, isolated, runFileLive, pid))
	}
	if runFileLive && probe.PID != pid {
		return fmt.Errorf("ao start: the AO daemon on port %d reports pid %d, not run-file pid %d, so its checkout identity cannot be trusted; %s", port, probe.PID, pid, devDaemonStopRemedy(runFile, isolated, runFileLive, probe.PID))
	}
	if !devDaemonMatchesCheckout(probe, root) {
		actual := probe.WorkingDirectory
		if actual == "" {
			actual = probe.ExecutablePath
		}
		if actual == "" {
			actual = "an unknown location"
		}
		return fmt.Errorf("ao start: another AO daemon is already running from %s (pid %d, port %d); expected this checkout at %s; %s", actual, probe.PID, port, root, devDaemonStopRemedy(runFile, isolated, runFileLive, probe.PID))
	}
	_, _ = fmt.Fprintf(w, "Using the AO daemon already running from this checkout (pid %d, port %d).\n", probe.PID, port)
	return nil
}

func devDaemonStopRemedy(runFile string, isolated, runFileLive bool, pid int) string {
	if !runFileLive {
		if isolated {
			return fmt.Sprintf("stop process pid %d manually; see docs/local-development.md for isolated daemon cleanup", pid)
		}
		return fmt.Sprintf("stop process pid %d manually, or set ISOLATE_DEV=true to use a separate data dir and port", pid)
	}
	if isolated {
		return fmt.Sprintf("stop it by running `ao stop` with "+
			"AO_RUN_FILE=%q and AO_DATA_DIR=%q set (`ao stop` does not read ISOLATE_DEV); "+
			"see docs/local-development.md.",
			runFile, filepath.Join(filepath.Dir(runFile), "data"))
	}
	return "stop it with `ao stop`, or set ISOLATE_DEV=true to use a separate data dir and port"
}

func devDaemonMatchesCheckout(probe probeResult, root string) bool {
	backend := filepath.Join(root, "backend")
	return probe.WorkingDirectory != "" && sameDevPath(probe.WorkingDirectory, backend) ||
		probe.ExecutablePath != "" && devPathInside(probe.ExecutablePath, backend)
}

func sameDevPath(a, b string) bool {
	return comparableDevPath(a) == comparableDevPath(b)
}

func devPathInside(child, parent string) bool {
	rel, err := filepath.Rel(comparableDevPath(parent), comparableDevPath(child))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func comparableDevPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}
