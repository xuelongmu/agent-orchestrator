package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
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

	if _, err := c.deps.LookPath("npm"); err != nil {
		return fmt.Errorf("ao start: running from source needs npm on PATH (install Node.js), or use `ao start --release`")
	}
	if _, err := os.Stat(filepath.Join(frontend, "node_modules")); err != nil {
		return fmt.Errorf("ao start: frontend dependencies are not installed; run `npm ci --prefix %s` first", frontend)
	}

	c.warnForeignDaemon(errOut, root)

	_, _ = fmt.Fprintf(out, "Starting Agent Orchestrator from source at %s\n", root)
	_, _ = fmt.Fprintf(out, "Running `npm run dev` in %s — press Ctrl-C to stop.\n", frontend)
	_, _ = fmt.Fprintf(out, "Use `ao start --release` to fetch and open the published desktop app instead.\n")

	return c.deps.RunAttached(ctx, frontend, "npm", "run", "dev")
}

// warnForeignDaemon prints a heads-up when a daemon is already live and did not
// come from this checkout. The desktop app enforces launch identity and will
// refuse to attach, which otherwise surfaces as a confusing in-app error.
func (c *commandContext) warnForeignDaemon(w io.Writer, root string) {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	info, err := runfile.CheckStale(cfg.RunFilePath)
	if err != nil || info == nil {
		return
	}
	_, _ = fmt.Fprintf(w,
		"Warning: an AO daemon is already running (pid %d, port %d).\n"+
			"If it is not from %s, quit that app first — the dev app will refuse to attach to it.\n",
		info.PID, info.Port, root)
}
