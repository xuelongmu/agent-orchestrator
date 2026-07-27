package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// makeCheckout builds a directory that satisfies isSourceCheckout, optionally
// with frontend/node_modules present. The returned path is symlink-resolved:
// checkout detection runs off os.Getwd, which resolves, and on macOS TMPDIR
// sits under /var -> /private/var.
func makeCheckout(t *testing.T, withNodeModules bool) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "backend"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "frontend"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "backend", "go.mod"), []byte(backendModuleLine+"\n\ngo 1.25.7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "frontend", "package.json"), []byte(`{"name":"agent-orchestrator"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if withNodeModules {
		if err := os.MkdirAll(filepath.Join(root, "frontend", "node_modules"), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// chdir moves into dir for the duration of the test.
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func TestResolveStartMode(t *testing.T) {
	tests := []struct {
		name      string
		requested startMode
		checkout  string
		devBuild  bool
		want      startMode
	}{
		{"auto dev build in checkout runs source", startModeAuto, "/repo", true, startModeSource},
		{"auto stamped build in checkout stays on release", startModeAuto, "/repo", false, startModeRelease},
		{"auto dev build outside a checkout stays on release", startModeAuto, "", true, startModeRelease},
		{"explicit source wins over a stamped build", startModeSource, "/repo", false, startModeSource},
		{"explicit release wins inside a checkout", startModeRelease, "/repo", true, startModeRelease},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveStartMode(tt.requested, tt.checkout, tt.devBuild); got != tt.want {
				t.Fatalf("resolveStartMode = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFindSourceCheckout_FromNestedDirectory(t *testing.T) {
	root := makeCheckout(t, false)
	nested := filepath.Join(root, "backend", "internal", "cli")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	if got := findSourceCheckout(nested); got != root {
		t.Fatalf("findSourceCheckout = %q, want %q", got, root)
	}
}

func TestFindSourceCheckout_RejectsNonAOTree(t *testing.T) {
	// Both halves present, but go.mod declares a different module: a coincidental
	// backend/ + frontend/ layout must not qualify.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "backend"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "frontend"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "backend", "go.mod"), []byte("module example.com/other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "frontend", "package.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := findSourceCheckout(root); got != "" {
		t.Fatalf("findSourceCheckout = %q, want \"\"", got)
	}
}

func TestStart_SourceRunsFrontendDevHarness(t *testing.T) {
	setConfigEnv(t)
	root := makeCheckout(t, true)
	chdir(t, root)

	var gotDir string
	var gotArgv []string
	deps := Deps{
		LookPath: func(string) (string, error) { return "/usr/bin/npm", nil },
		RunAttached: func(_ context.Context, dir, name string, args ...string) error {
			gotDir = dir
			gotArgv = append([]string{name}, args...)
			return nil
		},
	}

	out, _, err := executeCLI(t, deps, "start", "--source")
	if err != nil {
		t.Fatalf("start --source: %v", err)
	}
	wantDir := filepath.Join(root, "frontend")
	if gotDir != wantDir {
		t.Fatalf("ran in %q, want %q", gotDir, wantDir)
	}
	// The LookPath-resolved npm is what gets run, not the bare name: on Windows
	// the resolution determines whether a batch shim needs cmd.exe.
	wantName, wantArgs := devHarnessArgv("/usr/bin/npm")
	if want := append([]string{wantName}, wantArgs...); !reflect.DeepEqual(gotArgv, want) {
		t.Fatalf("argv = %v, want %v", gotArgv, want)
	}
	if !strings.Contains(out, root) {
		t.Fatalf("output does not name the checkout it started: %q", out)
	}
}

func TestStart_SourceWarnsWhenADaemonIsAlreadyRunning(t *testing.T) {
	cfg := setConfigEnv(t)
	root := makeCheckout(t, true)
	chdir(t, root)

	// A live PID makes runfile.CheckStale report the daemon as running.
	if err := runfile.Write(cfg.runFile, runfile.Info{
		PID: os.Getpid(), Port: 3001, StartedAt: time.Unix(100, 0).UTC(),
	}); err != nil {
		t.Fatalf("write run-file: %v", err)
	}

	deps := Deps{
		LookPath:    func(string) (string, error) { return "/usr/bin/npm", nil },
		RunAttached: func(context.Context, string, string, ...string) error { return nil },
	}

	_, errOut, err := executeCLI(t, deps, "start", "--source")
	if err != nil {
		t.Fatalf("start --source: %v", err)
	}
	// A dev launch attaches to an existing daemon rather than starting this
	// checkout's, so the warning must name the escape hatches, not claim the
	// app will refuse to attach.
	for _, want := range []string{"already running", "ao stop", "ISOLATE_DEV=true"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("warning %q missing %q", errOut, want)
		}
	}
}

func TestStart_SourceIsolatedIgnoresTheCanonicalDaemon(t *testing.T) {
	cfg := setConfigEnv(t)
	root := makeCheckout(t, true)
	chdir(t, root)
	// ISOLATE_DEV moves the dev launch to ~/.ao/dev, so a daemon on the
	// canonical run file is not the one it would attach to.
	t.Setenv("ISOLATE_DEV", "true")
	t.Setenv("AO_RUN_FILE", "")
	t.Setenv("HOME", t.TempDir())
	if err := runfile.Write(cfg.runFile, runfile.Info{
		PID: os.Getpid(), Port: 3001, StartedAt: time.Unix(100, 0).UTC(),
	}); err != nil {
		t.Fatalf("write run-file: %v", err)
	}

	deps := Deps{
		LookPath:    func(string) (string, error) { return "/usr/bin/npm", nil },
		RunAttached: func(context.Context, string, string, ...string) error { return nil },
	}

	_, errOut, err := executeCLI(t, deps, "start", "--source")
	if err != nil {
		t.Fatalf("start --source: %v", err)
	}
	if strings.Contains(errOut, "already running") {
		t.Fatalf("warned about the canonical daemon under ISOLATE_DEV: %q", errOut)
	}
}

func TestDevRunFilePath(t *testing.T) {
	home := t.TempDir()
	canonical := filepath.Join(home, ".ao", "running.json")

	t.Run("canonical when not isolated", func(t *testing.T) {
		t.Setenv("ISOLATE_DEV", "")
		t.Setenv("AO_RUN_FILE", "")
		got, isolated := devRunFilePath(canonical)
		if got != canonical || isolated {
			t.Fatalf("= %q isolated=%v, want %q false", got, isolated, canonical)
		}
	})

	t.Run("isolated moves under .ao/dev", func(t *testing.T) {
		t.Setenv("ISOLATE_DEV", "true")
		t.Setenv("AO_RUN_FILE", "")
		t.Setenv("HOME", home)
		want := filepath.Join(home, ".ao", "dev", "running.json")
		got, isolated := devRunFilePath(canonical)
		if got != want || !isolated {
			t.Fatalf("= %q isolated=%v, want %q true", got, isolated, want)
		}
	})

	t.Run("explicit AO_RUN_FILE wins over isolation", func(t *testing.T) {
		t.Setenv("ISOLATE_DEV", "true")
		t.Setenv("AO_RUN_FILE", filepath.Join(home, "custom.json"))
		got, isolated := devRunFilePath(canonical)
		if got != canonical || !isolated {
			t.Fatalf("= %q isolated=%v, want %q true", got, isolated, canonical)
		}
	})
}

func TestStart_SourceOutsideCheckoutIsUsageError(t *testing.T) {
	setConfigEnv(t)
	chdir(t, t.TempDir())

	deps := Deps{
		RunAttached: func(context.Context, string, string, ...string) error {
			t.Fatal("must not run the dev harness outside a checkout")
			return nil
		},
	}

	if _, _, err := executeCLI(t, deps, "start", "--source"); ExitCode(err) != 2 {
		t.Fatalf("exit code = %d, want 2 (usage); err = %v", ExitCode(err), err)
	}
}

func TestStart_SourceAndReleaseConflictIsUsageError(t *testing.T) {
	setConfigEnv(t)
	chdir(t, makeCheckout(t, true))

	if _, _, err := executeCLI(t, Deps{}, "start", "--source", "--release"); ExitCode(err) != 2 {
		t.Fatalf("exit code = %d, want 2 (usage); err = %v", ExitCode(err), err)
	}
}

func TestStart_SourceWithJSONIsUsageError(t *testing.T) {
	setConfigEnv(t)
	chdir(t, makeCheckout(t, true))

	deps := Deps{LookPath: func(string) (string, error) { return "/usr/bin/npm", nil }}
	if _, _, err := executeCLI(t, deps, "start", "--source", "--json"); ExitCode(err) != 2 {
		t.Fatalf("exit code = %d, want 2 (usage); err = %v", ExitCode(err), err)
	}
}

func TestStart_SourceWithoutFrontendDepsExplainsInstall(t *testing.T) {
	setConfigEnv(t)
	chdir(t, makeCheckout(t, false))

	deps := Deps{
		LookPath: func(string) (string, error) { return "/usr/bin/npm", nil },
		RunAttached: func(context.Context, string, string, ...string) error {
			t.Fatal("must not run the dev harness before deps are installed")
			return nil
		},
	}

	_, _, err := executeCLI(t, deps, "start", "--source")
	if err == nil || !strings.Contains(err.Error(), "npm ci --prefix") {
		t.Fatalf("error = %v, want npm ci guidance", err)
	}
}

func TestStart_SourceWithoutNPMExplainsRequirement(t *testing.T) {
	setConfigEnv(t)
	chdir(t, makeCheckout(t, true))

	deps := Deps{
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
		RunAttached: func(context.Context, string, string, ...string) error {
			t.Fatal("must not run the dev harness without npm")
			return nil
		},
	}

	_, _, err := executeCLI(t, deps, "start", "--source")
	if err == nil || !strings.Contains(err.Error(), "npm on PATH") {
		t.Fatalf("error = %v, want npm-on-PATH guidance", err)
	}
}
