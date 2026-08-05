package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
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

// offlineHTTPClient returns a client whose requests always fail, so the
// daemon-port probe in warnRunningDaemon cannot reach a real daemon on the
// developer's machine. Without it these tests would depend on host state.
func offlineHTTPClient() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	})}
}

// setHomeDir points os.UserHomeDir at dir for the duration of the test. It sets
// both variables because os.UserHomeDir reads USERPROFILE on Windows and HOME
// elsewhere; setting only HOME silently resolves the real profile on Windows.
func setHomeDir(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
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
		{"auto stamped build in checkout runs source", startModeAuto, "/repo", false, startModeSource},
		{"auto dev build outside a checkout refuses release", startModeAuto, "", true, startModeDevOutsideCheckout},
		{"auto stamped build outside a checkout runs release", startModeAuto, "", false, startModeRelease},
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

func TestStart_DevBuildOutsideCheckoutRequiresExplicitRelease(t *testing.T) {
	setConfigEnv(t)
	chdir(t, t.TempDir())

	deps := Deps{
		HTTPClient: offlineHTTPClient(),
		RunAttached: func(context.Context, string, string, ...string) error {
			t.Fatal("must not run source or install a release outside a checkout")
			return nil
		},
	}

	_, _, err := executeCLI(t, deps, "start")
	if ExitCode(err) != 2 {
		t.Fatalf("exit code = %d, want 2 (explicit choice required); err = %v", ExitCode(err), err)
	}
	if !strings.Contains(err.Error(), "refusing release mode") || !strings.Contains(err.Error(), "--release") {
		t.Fatalf("error = %v, want refusal and explicit --release guidance", err)
	}
}

func TestStart_AutoRunsCheckoutEvenForStampedBuild(t *testing.T) {
	setConfigEnv(t)
	root := makeCheckout(t, true)
	chdir(t, root)

	oldVersion := Version
	Version = "1.2.3"
	t.Cleanup(func() { Version = oldVersion })

	var ran bool
	deps := Deps{
		HTTPClient: offlineHTTPClient(),
		LookPath:   func(string) (string, error) { return "/usr/bin/npm", nil },
		RunAttached: func(context.Context, string, string, ...string) error {
			ran = true
			return nil
		},
	}

	if _, _, err := executeCLI(t, deps, "start"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !ran {
		t.Fatal("did not run the source harness from the checkout")
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
		HTTPClient: offlineHTTPClient(),
		LookPath:   func(string) (string, error) { return "/usr/bin/npm", nil },
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

func TestStart_SourceRespectsConfiguredDaemonOverride(t *testing.T) {
	setConfigEnv(t)
	root := makeCheckout(t, true)
	chdir(t, root)
	t.Setenv("AO_DAEMON_COMMAND", "custom-ao daemon")

	var ran bool
	deps := Deps{
		HTTPClient: offlineHTTPClient(),
		LookPath:   func(string) (string, error) { return "/usr/bin/npm", nil },
		RunAttached: func(context.Context, string, string, ...string) error {
			ran = true
			return nil
		},
	}

	_, errOut, err := executeCLI(t, deps, "start", "--source")
	if err != nil {
		t.Fatalf("start --source: %v", err)
	}
	if !ran || !strings.Contains(errOut, "explicitly configured daemon") {
		t.Fatalf("ran = %v, stderr = %q", ran, errOut)
	}
}

func TestStart_SourceReusesDaemonFromSameCheckout(t *testing.T) {
	cfg := setConfigEnv(t)
	root := makeCheckout(t, true)
	chdir(t, root)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(probeResult{
			Status: "ok", Service: daemonmeta.ServiceName, PID: os.Getpid(),
			WorkingDirectory: filepath.Join(root, "backend"),
		})
	}))
	t.Cleanup(srv.Close)

	if err := runfile.Write(cfg.runFile, runfile.Info{
		PID: os.Getpid(), Port: serverPort(t, srv.URL), StartedAt: time.Unix(100, 0).UTC(),
	}); err != nil {
		t.Fatalf("write run-file: %v", err)
	}

	deps := Deps{
		HTTPClient:  srv.Client(),
		LookPath:    func(string) (string, error) { return "/usr/bin/npm", nil },
		RunAttached: func(context.Context, string, string, ...string) error { return nil },
	}

	_, errOut, err := executeCLI(t, deps, "start", "--source")
	if err != nil {
		t.Fatalf("start --source: %v", err)
	}
	for _, want := range []string{"Using the AO daemon", "this checkout"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("output %q missing %q", errOut, want)
		}
	}
}

func TestStart_SourceFallsBackFromPIDInconsistentRunFile(t *testing.T) {
	cfg := setConfigEnv(t)
	root := makeCheckout(t, true)
	chdir(t, root)

	staleWorkingDirectory := filepath.Join(t.TempDir(), "other", "backend")
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(probeResult{
			Status: "ok", Service: daemonmeta.ServiceName, PID: 31337,
			WorkingDirectory: staleWorkingDirectory,
		})
	}))
	t.Cleanup(stale.Close)

	expected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(probeResult{
			Status: "ok", Service: daemonmeta.ServiceName, PID: 4242,
			WorkingDirectory: filepath.Join(root, "backend"),
		})
	}))
	t.Cleanup(expected.Close)
	expectedPort := serverPort(t, expected.URL)
	t.Setenv("AO_PORT", strconv.Itoa(expectedPort))

	if err := runfile.Write(cfg.runFile, runfile.Info{
		PID: os.Getpid(), Port: serverPort(t, stale.URL), StartedAt: time.Unix(100, 0).UTC(),
	}); err != nil {
		t.Fatalf("write run-file: %v", err)
	}

	deps := Deps{
		HTTPClient:  expected.Client(),
		LookPath:    func(string) (string, error) { return "/usr/bin/npm", nil },
		RunAttached: func(context.Context, string, string, ...string) error { return nil },
	}

	_, errOut, err := executeCLI(t, deps, "start", "--source")
	if err != nil {
		t.Fatalf("start --source: %v", err)
	}
	if !strings.Contains(errOut, "pid 4242") || !strings.Contains(errOut, fmt.Sprintf("port %d", expectedPort)) {
		t.Fatalf("output = %q, want configured-port daemon identity", errOut)
	}
}

func TestStart_SourceRejectsDifferentDaemonWhenOnlyThePortAnswers(t *testing.T) {
	// No run file at all, but a daemon holds the port. Electron attaches in
	// exactly this case through main.ts's direct-port fallback, so rejection must
	// not depend on the run file being readable.
	otherWorkingDirectory := filepath.Join(t.TempDir(), "packaged", "resources")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(probeResult{
			Status: "ok", Service: daemonmeta.ServiceName, PID: 4242,
			WorkingDirectory: otherWorkingDirectory,
		})
	}))
	t.Cleanup(srv.Close)

	setConfigEnv(t)
	t.Setenv("AO_PORT", strconv.Itoa(serverPort(t, srv.URL)))
	root := makeCheckout(t, true)
	chdir(t, root)

	deps := Deps{
		HTTPClient: srv.Client(),
		LookPath:   func(string) (string, error) { return "/usr/bin/npm", nil },
		RunAttached: func(context.Context, string, string, ...string) error {
			t.Fatal("must not launch the dev harness with a mismatched daemon")
			return nil
		},
	}

	_, _, err := executeCLI(t, deps, "start", "--source")
	if err == nil || !strings.Contains(err.Error(), "another AO daemon") || !strings.Contains(err.Error(), "pid 4242") {
		t.Fatalf("error = %v, want mismatched port-only daemon refusal", err)
	}
	if strings.Contains(err.Error(), "ao stop") || !strings.Contains(err.Error(), "stop process pid 4242 manually") {
		t.Fatalf("error = %v, want a PID-aware remedy without ao stop", err)
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
	setHomeDir(t, t.TempDir())
	if err := runfile.Write(cfg.runFile, runfile.Info{
		PID: os.Getpid(), Port: 3001, StartedAt: time.Unix(100, 0).UTC(),
	}); err != nil {
		t.Fatalf("write run-file: %v", err)
	}

	deps := Deps{
		HTTPClient:  offlineHTTPClient(),
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

func TestStart_SourceIsolatedRemedyScopesTheStop(t *testing.T) {
	setConfigEnv(t)
	root := makeCheckout(t, true)
	chdir(t, root)
	home := t.TempDir()
	t.Setenv("ISOLATE_DEV", "true")
	t.Setenv("AO_RUN_FILE", "")
	setHomeDir(t, home)

	// A live daemon on the isolated run file is what an isolated launch attaches to.
	isolatedRunFile := filepath.Join(home, ".ao", "dev", "running.json")
	if err := os.MkdirAll(filepath.Dir(isolatedRunFile), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := runfile.Write(isolatedRunFile, runfile.Info{
		PID: os.Getpid(), Port: 3002, StartedAt: time.Unix(100, 0).UTC(),
	}); err != nil {
		t.Fatalf("write run-file: %v", err)
	}

	deps := Deps{
		HTTPClient:  offlineHTTPClient(),
		LookPath:    func(string) (string, error) { return "/usr/bin/npm", nil },
		RunAttached: func(context.Context, string, string, ...string) error { return nil },
	}

	_, _, err := executeCLI(t, deps, "start", "--source")
	if err == nil {
		t.Fatal("start --source succeeded with an unverified isolated daemon")
	}
	// Plain `ao stop` resolves through config.Load, which ignores ISOLATE_DEV and
	// would stop the canonical daemon instead, so the remedy must name the
	// isolated paths. It quotes them rather than emitting a shell command: a
	// POSIX `VAR=x cmd` prefix is not valid in PowerShell or cmd.exe.
	if !strings.Contains(err.Error(), strconv.Quote(isolatedRunFile)) {
		t.Fatalf("remedy does not name the isolated run file: %q", err)
	}
	if !strings.Contains(err.Error(), strconv.Quote(filepath.Join(home, ".ao", "dev", "data"))) {
		t.Fatalf("remedy does not name the isolated data dir: %q", err)
	}
	if strings.Contains(err.Error(), "AO_RUN_FILE="+isolatedRunFile) {
		t.Fatalf("remedy emits a POSIX-only env prefix: %q", err)
	}
}

func TestDevDaemonMatchesCheckout(t *testing.T) {
	root := makeCheckout(t, false)
	tests := []struct {
		name  string
		probe probeResult
		want  bool
	}{
		{"backend working directory", probeResult{WorkingDirectory: filepath.Join(root, "backend")}, true},
		{"binary inside backend", probeResult{ExecutablePath: filepath.Join(root, "backend", "bin", "ao")}, true},
		{"other checkout", probeResult{WorkingDirectory: filepath.Join(t.TempDir(), "backend")}, false},
		{"packaged binary", probeResult{ExecutablePath: filepath.Join(t.TempDir(), "resources", "daemon", "ao")}, false},
		{"missing identity", probeResult{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := devDaemonMatchesCheckout(tt.probe, root); got != tt.want {
				t.Fatalf("devDaemonMatchesCheckout = %v, want %v", got, tt.want)
			}
		})
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
		setHomeDir(t, home)
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
		HTTPClient: offlineHTTPClient(),
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

	deps := Deps{LookPath: func(string) (string, error) { return "/usr/bin/npm", nil }, HTTPClient: offlineHTTPClient()}
	if _, _, err := executeCLI(t, deps, "start", "--source", "--json"); ExitCode(err) != 2 {
		t.Fatalf("exit code = %d, want 2 (usage); err = %v", ExitCode(err), err)
	}
}

func TestStart_SourceWithoutFrontendDepsExplainsInstall(t *testing.T) {
	setConfigEnv(t)
	chdir(t, makeCheckout(t, false))

	deps := Deps{
		HTTPClient: offlineHTTPClient(),
		LookPath:   func(string) (string, error) { return "/usr/bin/npm", nil },
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
		HTTPClient: offlineHTTPClient(),
		LookPath:   func(string) (string, error) { return "", errors.New("not found") },
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
