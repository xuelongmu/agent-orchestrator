package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDoctorChecksGitVersion(t *testing.T) {
	setConfigEnv(t)
	c := doctorContext(t, map[string]string{"git": "/bin/git"}, func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "/bin/git" || len(args) != 1 || args[0] != "--version" {
			t.Fatalf("unexpected command: %s %v", name, args)
		}
		return []byte("git version 2.43.0\n"), nil
	})

	check := findDoctorCheck(t, c.runDoctor(context.Background()), "git")
	if check.Level != doctorPass || !strings.Contains(check.Message, "2.43.0") || !strings.Contains(check.Message, "supports worktrees") {
		t.Fatalf("git check = %+v, want PASS with version", check)
	}
}

func TestDoctorWarnsOnUnsupportedGitVersion(t *testing.T) {
	setConfigEnv(t)
	c := doctorContext(t, map[string]string{"git": "/bin/git"}, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("git version 2.24.9\n"), nil
	})

	check := findDoctorCheck(t, c.runDoctor(context.Background()), "git")
	if check.Level != doctorWarn || !strings.Contains(check.Message, ">= 2.25.0") {
		t.Fatalf("git check = %+v, want WARN with minimum version", check)
	}
}

func TestDoctorFailsWhenGitMissing(t *testing.T) {
	setConfigEnv(t)
	c := doctorContext(t, map[string]string{}, nil)

	check := findDoctorCheck(t, c.runDoctor(context.Background()), "git")
	if check.Level != doctorFail {
		t.Fatalf("git check = %+v, want FAIL", check)
	}
}

func TestDoctorChecksTmuxVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ao doctor emits a conpty check on Windows, not tmux")
	}
	setConfigEnv(t)
	c := doctorContext(t, map[string]string{"git": "/bin/git", "tmux": "/bin/tmux"}, func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "/bin/git":
			return []byte("git version 2.43.0\n"), nil
		case "/bin/tmux":
			if len(args) != 1 || args[0] != "-V" {
				t.Fatalf("unexpected tmux command: %s %v", name, args)
			}
			return []byte("tmux 3.3a\n"), nil
		default:
			t.Fatalf("unexpected command: %s %v", name, args)
			return nil, nil
		}
	})

	check := findDoctorCheck(t, c.runDoctor(context.Background()), "tmux")
	if check.Level != doctorPass || !strings.Contains(check.Message, "3.3a") {
		t.Fatalf("tmux check = %+v, want PASS with version", check)
	}
}

// TestDoctorChecksTmuxVersionFailsOnError covers the case where tmux is found
// but the version command fails.
func TestDoctorChecksTmuxVersionFailsOnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ao doctor emits a conpty check on Windows, not tmux")
	}
	setConfigEnv(t)
	c := doctorContext(t, map[string]string{"git": "/bin/git", "tmux": "/bin/tmux"}, func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name == "/bin/git" {
			return []byte("git version 2.43.0\n"), nil
		}
		return nil, errors.New("exec: tmux: not found")
	})

	check := findDoctorCheck(t, c.runDoctor(context.Background()), "tmux")
	if check.Level != doctorFail {
		t.Fatalf("tmux check = %+v, want FAIL on version error", check)
	}
}

func TestDoctorWarnsWhenTmuxMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ao doctor emits a conpty check on Windows, not tmux")
	}
	setConfigEnv(t)
	c := doctorContext(t, map[string]string{"git": "/bin/git"}, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("git version 2.43.0\n"), nil
	})

	check := findDoctorCheck(t, c.runDoctor(context.Background()), "tmux")
	if check.Level != doctorWarn {
		t.Fatalf("tmux check = %+v, want WARN", check)
	}
}

func TestDoctorChecksHarnessVersions(t *testing.T) {
	setConfigEnv(t)
	cmdPath := map[string]string{
		"git":    "/bin/git",
		"claude": "/bin/claude",
		"codex":  "/bin/codex",
	}
	c := doctorContext(t, cmdPath, func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "/bin/git":
			return []byte("git version 2.43.0\n"), nil
		case "/bin/claude", "/bin/codex":
			if len(args) == 1 && args[0] == "--version" {
				return []byte(strings.TrimPrefix(name, "/bin/") + " 1.2.3\n"), nil
			}
			// The codex launch-flag canary probes the same binary.
			if name == "/bin/codex" && len(args) > 0 && (args[0] == "--dangerously-bypass-hook-trust" || args[0] == "features") {
				return []byte("ok\n"), nil
			}
			t.Fatalf("unexpected harness command: %s %v", name, args)
			return nil, nil
		default:
			t.Fatalf("unexpected command: %s %v", name, args)
			return nil, nil
		}
	})

	checks := c.runDoctor(context.Background())
	for _, name := range []string{"claude-code", "codex"} {
		check := findDoctorCheck(t, checks, name)
		if check.Level != doctorPass || !strings.Contains(check.Message, "resolves to") {
			t.Fatalf("%s check = %+v, want PASS with path/version", name, check)
		}
	}
}

func TestDoctorWarnsWhenHarnessMissing(t *testing.T) {
	setConfigEnv(t)
	c := doctorContext(t, map[string]string{"git": "/bin/git"}, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("git version 2.43.0\n"), nil
	})

	check := findDoctorCheck(t, c.runDoctor(context.Background()), "codex")
	if check.Level != doctorWarn || !strings.Contains(check.Message, "not found in PATH") {
		t.Fatalf("codex check = %+v, want WARN missing binary", check)
	}
}

func TestDoctorWarnsWhenHarnessVersionFails(t *testing.T) {
	setConfigEnv(t)
	c := doctorContext(t, map[string]string{"git": "/bin/git", "codex": "/bin/codex"}, func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name == "/bin/git" {
			return []byte("git version 2.43.0\n"), nil
		}
		return nil, errors.New("boom")
	})

	check := findDoctorCheck(t, c.runDoctor(context.Background()), "codex")
	if check.Level != doctorWarn || !strings.Contains(check.Message, "failed") {
		t.Fatalf("codex check = %+v, want WARN version failure", check)
	}
}

func TestDoctorChecksGitHubTokenFromEnv(t *testing.T) {
	setConfigEnv(t)
	srv := githubDoctorServer(t, http.StatusOK, `{"login":"octocat"}`, "repo, read:org")
	c := doctorContext(t, map[string]string{"git": "/bin/git"}, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("git version 2.43.0\n"), nil
	})
	t.Setenv("AO_GITHUB_TOKEN", "env-token")
	c.deps.HTTPClient = srv.Client()
	c.deps.DoctorGitHubRESTBase = srv.URL

	check := findDoctorCheck(t, c.runDoctor(context.Background()), "github-token")
	if check.Level != doctorPass || !strings.Contains(check.Message, "AO_GITHUB_TOKEN") || !strings.Contains(check.Message, "repo, read:org") {
		t.Fatalf("github-token check = %+v, want PASS with source and scopes", check)
	}
}

func TestDoctorChecksGitHubTokenFromGHCLI(t *testing.T) {
	setConfigEnv(t)
	srv := githubDoctorServer(t, http.StatusOK, `{"login":"octocat"}`, "")
	c := doctorContext(t, map[string]string{"git": "/bin/git", "gh": "/bin/gh"}, func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "/bin/gh" {
			if len(args) != 2 || args[0] != "auth" || args[1] != "token" {
				t.Fatalf("unexpected gh command: %s %v", name, args)
			}
			return []byte("gh-token\n"), nil
		}
		return []byte("git version 2.43.0\n"), nil
	})
	c.deps.HTTPClient = srv.Client()
	c.deps.DoctorGitHubRESTBase = srv.URL

	check := findDoctorCheck(t, c.runDoctor(context.Background()), "github-token")
	if check.Level != doctorPass || !strings.Contains(check.Message, "gh token valid") {
		t.Fatalf("github-token check = %+v, want PASS from gh", check)
	}
}

func TestDoctorWarnsWhenGitHubTokenMissing(t *testing.T) {
	setConfigEnv(t)
	c := doctorContext(t, map[string]string{"git": "/bin/git"}, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("git version 2.43.0\n"), nil
	})

	check := findDoctorCheck(t, c.runDoctor(context.Background()), "github-token")
	if check.Level != doctorWarn || !strings.Contains(check.Message, "no GitHub token found") {
		t.Fatalf("github-token check = %+v, want WARN missing token", check)
	}
}

func TestDoctorFailsExpiredGitHubToken(t *testing.T) {
	setConfigEnv(t)
	srv := githubDoctorServer(t, http.StatusUnauthorized, `{"message":"Bad credentials"}`, "")
	c := doctorContext(t, map[string]string{"git": "/bin/git"}, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("git version 2.43.0\n"), nil
	})
	t.Setenv("GITHUB_TOKEN", "expired-token")
	c.deps.HTTPClient = srv.Client()
	c.deps.DoctorGitHubRESTBase = srv.URL

	check := findDoctorCheck(t, c.runDoctor(context.Background()), "github-token")
	if check.Level != doctorFail || !strings.Contains(check.Message, "HTTP 401") {
		t.Fatalf("github-token check = %+v, want FAIL rejected token", check)
	}
}

func TestDoctorJSONOutputIsDecodable(t *testing.T) {
	setConfigEnv(t)
	clearDoctorGitHubEnv(t)
	out, errOut, err := executeCLI(t, Deps{
		LookPath: func(name string) (string, error) {
			switch name {
			case "git":
				return "/bin/git", nil
			case "tmux":
				return "/bin/tmux", nil
			}
			return "", errors.New("missing")
		},
		CommandOutput: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if name == "/bin/tmux" {
				return []byte("tmux 3.3a\n"), nil
			}
			return []byte("git version 2.43.0\n"), nil
		},
		ProcessAlive: func(int) bool { return false },
	}, "doctor", "--json")
	if err != nil {
		t.Fatalf("doctor --json failed: %v\nstderr=%s\nstdout=%s", err, errOut, out)
	}
	var got doctorReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode doctor json: %v\nout=%s", err, out)
	}
	if !got.OK || len(got.Checks) == 0 {
		t.Fatalf("doctor json = %#v, want ok with checks", got)
	}
	if findDoctorCheck(t, got.Checks, "git").Section != doctorSectionTools {
		t.Fatalf("git json check missing section: %#v", findDoctorCheck(t, got.Checks, "git"))
	}
}

func TestDoctorTextOutputIsGrouped(t *testing.T) {
	setConfigEnv(t)
	clearDoctorGitHubEnv(t)
	out, errOut, err := executeCLI(t, Deps{
		LookPath: func(name string) (string, error) {
			switch name {
			case "git":
				return "/bin/git", nil
			case "tmux":
				return "/bin/tmux", nil
			}
			return "", errors.New("missing")
		},
		CommandOutput: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if name == "/bin/tmux" {
				return []byte("tmux 3.3a\n"), nil
			}
			return []byte("git version 2.43.0\n"), nil
		},
		ProcessAlive: func(int) bool { return false },
	}, "doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\nstderr=%s\nstdout=%s", err, errOut, out)
	}
	for _, want := range []string{"Core:\nPASS config:", "Tools:\nPASS git:", "Agent harnesses:\nWARN claude-code:", "WARN codex:", "GitHub:\nWARN github-token:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out)
		}
	}
}

func clearDoctorGitHubEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AO_GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
}

// TestDoctorChecksAOBinaryIdentity covers the `ao-binary` check: workspace
// hooks invoke a bare `ao hooks <agent> <event>`, so doctor must surface when
// the `ao` on PATH is not the running binary (e.g. a legacy CLI without the
// hooks command shadowing the Go one).
func TestDoctorChecksAOBinaryIdentity(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "ao")
	other := filepath.Join(dir, "ao-legacy")
	for _, p := range []string{self, other} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // test fixture must be executable-shaped
			t.Fatal(err)
		}
	}
	selfExe := func() (string, error) { return self, nil }

	cases := []struct {
		name       string
		executable func() (string, error)
		paths      map[string]string
		wantLevel  doctorLevel
		wantIn     string
	}{
		{"ao in PATH is this binary", selfExe, map[string]string{"ao": self}, doctorPass, "this binary"},
		{"ao in PATH is a different binary", selfExe, map[string]string{"ao": other}, doctorWarn, "not this binary"},
		{"ao missing from PATH", selfExe, map[string]string{}, doctorWarn, "not found in PATH"},
		{"running executable unresolvable", func() (string, error) { return "", errors.New("no exe") }, map[string]string{"ao": self}, doctorWarn, "could not resolve"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := Deps{
				Executable: tc.executable,
				LookPath: func(name string) (string, error) {
					path, ok := tc.paths[name]
					if !ok || path == "" {
						return "", fmt.Errorf("%s missing", name)
					}
					return path, nil
				},
				ProcessAlive: func(int) bool { return false },
			}
			c := &commandContext{deps: deps.withDefaults()}
			check := c.checkAOBinary(context.Background())
			if check.Level != tc.wantLevel || !strings.Contains(check.Message, tc.wantIn) {
				t.Fatalf("ao-binary check = %+v, want level %s with %q", check, tc.wantLevel, tc.wantIn)
			}
		})
	}
}

// TestDoctorFailsLegacyWindowsAOShadow is the install/update regression for a
// stale fnm/npm shim resolving before the desktop app's bundled Go CLI. Keep
// the platform explicit so Linux CI exercises the Windows decision too.
func TestDoctorFailsLegacyWindowsAOShadow(t *testing.T) {
	root := t.TempDir()
	canonical := filepath.Join(root, "desktop", "resources", "daemon", "ao.exe")
	shadow := writeWindowsAOShimFixture(t, filepath.Join(root, "fnm_multishells", "123"), "0.9.3")
	deps := Deps{
		Executable: func() (string, error) { return canonical, nil },
		LookPath: func(name string) (string, error) {
			if name != "ao" {
				t.Fatalf("LookPath(%q), want ao", name)
			}
			return shadow, nil
		},
		CommandOutput: func(_ context.Context, name string, args ...string) ([]byte, error) {
			t.Fatalf("doctor executed untrusted PATH content: %s %v", name, args)
			return nil, nil
		},
	}
	c := &commandContext{deps: deps.withDefaults()}

	preflight := c.checkAOBinaryForPlatform(context.Background(), "windows")
	if preflight.Level != doctorFail {
		t.Fatalf("ao-binary preflight = %+v, want FAIL", preflight)
	}
	checks := c.runDoctorForPlatform(context.Background(), "windows")
	if len(checks) != 1 {
		t.Fatalf("doctor checks = %+v, want immediate refusal before state/tool probes", checks)
	}
	check := checks[0]
	if check.Level != doctorFail {
		t.Fatalf("ao-binary check = %+v, want FAIL", check)
	}
	for _, want := range []string{"0.9.3", shadow, canonical, "canonical migrated entry point", "~/.agent-orchestrator", "~/.ao"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("ao-binary message %q missing %q", check.Message, want)
		}
	}
}

func TestDoctorWindowsAODifferentExeStaysWarningWithoutExecution(t *testing.T) {
	root := t.TempDir()
	canonical := filepath.Join(root, "desktop", "ao.exe")
	other := filepath.Join(root, "other", "ao.exe")
	for _, path := range []string{canonical, other} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o755); err != nil { //nolint:gosec // executable-shaped test fixture
			t.Fatal(err)
		}
	}
	c := &commandContext{deps: Deps{
		Executable: func() (string, error) { return canonical, nil },
		LookPath:   func(string) (string, error) { return other, nil },
		CommandOutput: func(_ context.Context, name string, args ...string) ([]byte, error) {
			t.Fatalf("doctor executed a different ao.exe: %s %v", name, args)
			return nil, nil
		},
	}.withDefaults()}

	check := c.checkAOBinaryForPlatform(context.Background(), "windows")
	if check.Level != doctorWarn || !strings.Contains(check.Message, "different installation") {
		t.Fatalf("ao-binary check = %+v, want WARN for different exe", check)
	}
}

func TestDoctorWindowsMigratedBootstrapStaysWarning(t *testing.T) {
	root := t.TempDir()
	shadow := writeWindowsAOShimFixtureWithEntry(t, filepath.Join(root, "fnm", "123"), "0.10.3", "bin/ao.js")
	writeWindowsAOPackageMetadata(t, shadow, `{"name":"@aoagents/ao","version":"0.10.3","bin":{"ao":"./bin/ao.js"},"optionalDependencies":{"@aoagents/ao-win32-x64":"0.10.3"}}`)
	c := &commandContext{deps: Deps{
		Executable: func() (string, error) { return filepath.Join(root, "canonical", "ao.exe"), nil },
		LookPath:   func(string) (string, error) { return shadow, nil },
		CommandOutput: func(_ context.Context, name string, args ...string) ([]byte, error) {
			t.Fatalf("doctor executed migrated bootstrap: %s %v", name, args)
			return nil, nil
		},
	}.withDefaults()}

	check := c.checkAOBinaryForPlatform(context.Background(), "windows")
	if check.Level != doctorWarn || !strings.Contains(check.Message, "migrated npm bootstrap") || !strings.Contains(check.Message, "0.10.3") {
		t.Fatalf("ao-binary check = %+v, want WARN for different migrated bootstrap", check)
	}
}

func TestDoctorFailsLegacyWrapperDespiteLaterVersion(t *testing.T) {
	root := t.TempDir()
	shadow := writeWindowsAOShimFixtureWithEntry(t, filepath.Join(root, "fnm", "legacy-wrapper"), "0.10.1-nightly-249c", "bin/ao.js")
	writeWindowsAOPackageMetadata(t, shadow, `{"name":"@aoagents/ao","version":"0.10.1-nightly-249c","bin":{"ao":"bin/ao.js"},"dependencies":{"@aoagents/ao-cli":"0.10.1-nightly-249c"}}`)
	c := &commandContext{deps: Deps{
		Executable: func() (string, error) { return filepath.Join(root, "canonical", "ao.exe"), nil },
		LookPath:   func(string) (string, error) { return shadow, nil },
		CommandOutput: func(_ context.Context, name string, args ...string) ([]byte, error) {
			t.Fatalf("doctor executed legacy wrapper: %s %v", name, args)
			return nil, nil
		},
	}.withDefaults()}

	check := c.checkAOBinaryForPlatform(context.Background(), "windows")
	if check.Level != doctorFail || !strings.Contains(check.Message, "0.10.1-nightly-249c") {
		t.Fatalf("ao-binary check = %+v, want FAIL for later-version legacy wrapper", check)
	}
}

func TestDoctorFailsNPMGeneratedLegacyShim(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "npm-bin")
	packageDir := filepath.Join(shimDir, "node_modules", "@aoagents", "ao-cli")
	entry := filepath.Join(packageDir, "dist", "index.js")
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("// fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "package.json"), []byte(`{"name":"@aoagents/ao-cli","version":"0.10.1-nightly-249c","bin":{"ao":"dist/index.js"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(shimDir, "ao.cmd")
	content := `@ECHO off
GOTO start
:find_dp0
SET dp0=%~dp0
EXIT /b
:start
SETLOCAL
CALL :find_dp0

IF EXIST "%dp0%\node.exe" (
  SET "_prog=%dp0%\node.exe"
) ELSE (
  SET "_prog=node"
  SET PATHEXT=%PATHEXT:;.JS;=;%
)

endLocal & goto #_undefined_# 2>NUL || title %COMSPEC% & "%_prog%"  "%dp0%\node_modules\@aoagents\ao-cli\dist\index.js" %*
`
	if err := os.WriteFile(shim, []byte(strings.ReplaceAll(content, "\n", "\r\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &commandContext{deps: Deps{
		Executable: func() (string, error) { return filepath.Join(root, "canonical", "ao.exe"), nil },
		LookPath:   func(string) (string, error) { return shim, nil },
		CommandOutput: func(_ context.Context, name string, args ...string) ([]byte, error) {
			t.Fatalf("doctor executed npm-generated legacy shim: %s %v", name, args)
			return nil, nil
		},
	}.withDefaults()}

	check := c.checkAOBinaryForPlatform(context.Background(), "windows")
	if check.Level != doctorFail || !strings.Contains(check.Message, "0.10.1-nightly-249c") {
		t.Fatalf("ao-binary check = %+v, want FAIL for npm-generated legacy shim", check)
	}
}

func TestInspectWindowsAOShimRejectsUNCAndDeviceEntriesBeforeIO(t *testing.T) {
	root := t.TempDir()
	originalStat, originalOpen := statAOPath, openAOPath
	t.Cleanup(func() {
		statAOPath = originalStat
		openAOPath = originalOpen
	})

	cases := []string{
		`\\server\share\ao\dist\index.js`,
		`//server/share/ao/dist/index.js`,
		`\\?\C:\ao\dist\index.js`,
		`\\.\C:\ao\dist\index.js`,
		`\??\C:\ao\dist\index.js`,
	}
	for i, entry := range cases {
		shim := filepath.Join(root, fmt.Sprintf("unsafe-%d.cmd", i))
		content := `@"%~dp0node.exe" "` + entry + `" %*` + "\r\n"
		if err := os.WriteFile(shim, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}

		var touched []string
		statAOPath = func(path string) (os.FileInfo, error) {
			touched = append(touched, "stat:"+path)
			return originalStat(path)
		}
		openAOPath = func(path string) (*os.File, error) {
			touched = append(touched, "open:"+path)
			return originalOpen(path)
		}

		if _, _, err := inspectWindowsAOShim(shim); err == nil || !strings.Contains(err.Error(), "not inspected") {
			t.Fatalf("inspectWindowsAOShim(%q) error = %v, want namespace refusal", entry, err)
		}
		if len(touched) != 1 || touched[0] != "open:"+shim {
			t.Fatalf("inspectWindowsAOShim(%q) filesystem touches = %v, want only local shim read", entry, touched)
		}
	}
}

func TestDoctorWindowsAOMalformedOrUntrustedShimStaysWarning(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name    string
		content string
	}{
		{"shell metacharacter", `@"%~dp0node.exe" "%~dp0node_modules\@aoagents\ao-cli\dist\index.js" %* & calc.exe`},
		{"powershell launcher", `powershell -File "%~dp0ao.ps1" %*`},
		{"arbitrary relative entry", `@"%~dp0node.exe" "..\..\untrusted.js" %*`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(root, strings.ReplaceAll(tc.name, " ", "-"))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			shim := filepath.Join(dir, "ao.cmd")
			if err := os.WriteFile(shim, []byte(tc.content+"\r\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			c := &commandContext{deps: Deps{
				Executable: func() (string, error) { return filepath.Join(root, "canonical", "ao.exe"), nil },
				LookPath:   func(string) (string, error) { return shim, nil },
				CommandOutput: func(_ context.Context, name string, args ...string) ([]byte, error) {
					t.Fatalf("doctor executed malformed shim: %s %v", name, args)
					return nil, nil
				},
			}.withDefaults()}

			check := c.checkAOBinaryForPlatform(context.Background(), "windows")
			if check.Level != doctorWarn || !strings.Contains(check.Message, "could not safely inspect") {
				t.Fatalf("ao-binary check = %+v, want safe WARN", check)
			}
		})
	}

	t.Run("untrusted package metadata", func(t *testing.T) {
		shimDir := filepath.Join(root, "untrusted-package", "fnm", "123")
		shim := writeWindowsAOShimFixture(t, shimDir, "0.9.3")
		packagePath := filepath.Join(filepath.Dir(filepath.Dir(shimDir)), "agent-orchestrator", "packages", "cli", "package.json")
		pkg := `{"name":"not-ao","version":"0.9.3","bin":{"ao":"dist/index.js"}}`
		if err := os.WriteFile(packagePath, []byte(pkg), 0o644); err != nil {
			t.Fatal(err)
		}
		c := &commandContext{deps: Deps{
			Executable: func() (string, error) { return filepath.Join(root, "canonical", "ao.exe"), nil },
			LookPath:   func(string) (string, error) { return shim, nil },
			CommandOutput: func(_ context.Context, name string, args ...string) ([]byte, error) {
				t.Fatalf("doctor executed untrusted package: %s %v", name, args)
				return nil, nil
			},
		}.withDefaults()}

		check := c.checkAOBinaryForPlatform(context.Background(), "windows")
		if check.Level != doctorWarn || !strings.Contains(check.Message, `untrusted package name "not-ao"`) {
			t.Fatalf("ao-binary check = %+v, want untrusted-package WARN", check)
		}
	})
}

func writeWindowsAOShimFixture(t *testing.T, shimDir, version string) string {
	t.Helper()
	return writeWindowsAOShimFixtureWithEntry(t, shimDir, version, "dist/index.js")
}

func writeWindowsAOShimFixtureWithEntry(t *testing.T, shimDir, version, entryRelative string) string {
	t.Helper()
	// Match the observed fnm dogfood layout: the ephemeral shim directory owns
	// node.exe while ao.cmd points at the absolute entry in the legacy checkout.
	packageDir := filepath.Join(filepath.Dir(filepath.Dir(shimDir)), "agent-orchestrator", "packages", "cli")
	entryParts := strings.FieldsFunc(entryRelative, func(r rune) bool { return r == '/' || r == '\\' })
	entry := filepath.Join(append([]string{packageDir}, entryParts...)...)
	for _, dir := range []string{shimDir, filepath.Dir(entry), packageDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(shimDir, "node.exe"), []byte("fixture"), 0o755); err != nil { //nolint:gosec // executable-shaped test fixture
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("// fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg := fmt.Sprintf(`{"name":"@aoagents/ao-cli","version":%q,"bin":{"ao":%q}}`, version, entryRelative)
	if err := os.WriteFile(filepath.Join(packageDir, "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(shimDir, "ao.cmd")
	// This is the production fnm shape: native node beside the shim, an absolute
	// package JS entry, and forwarded user arguments. Doctor reads but never runs it.
	entryForShim := strings.ReplaceAll(entry, string(filepath.Separator), `\`)
	content := `@"%~dp0node.exe" "` + entryForShim + `" %*` + "\r\n"
	if err := os.WriteFile(shim, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return shim
}

func writeWindowsAOPackageMetadata(t *testing.T, shim, metadata string) {
	t.Helper()
	shimDir := filepath.Dir(shim)
	packagePath := filepath.Join(filepath.Dir(filepath.Dir(shimDir)), "agent-orchestrator", "packages", "cli", "package.json")
	if err := os.WriteFile(packagePath, []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDoctorIncludesAOBinaryCheck asserts runDoctor actually surfaces the
// ao-binary check, so the identity probe cannot silently fall out of the report.
func TestDoctorIncludesAOBinaryCheck(t *testing.T) {
	setConfigEnv(t)
	c := doctorContext(t, map[string]string{"git": "/bin/git"}, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("git version 2.43.0\n"), nil
	})

	// doctorContext's LookPath has no "ao", so the check lands as a WARN.
	check := findDoctorCheck(t, c.runDoctor(context.Background()), "ao-binary")
	if check.Level != doctorWarn || !strings.Contains(check.Message, "not found in PATH") {
		t.Fatalf("ao-binary check = %+v, want WARN for missing ao", check)
	}
}

func doctorContext(t *testing.T, paths map[string]string, commandOutput func(context.Context, string, ...string) ([]byte, error)) *commandContext {
	t.Helper()
	clearDoctorGitHubEnv(t)
	deps := Deps{
		LookPath: func(name string) (string, error) {
			path, ok := paths[name]
			if !ok || path == "" {
				return "", fmt.Errorf("%s missing", name)
			}
			return path, nil
		},
		ProcessAlive: func(int) bool { return false },
	}
	if commandOutput != nil {
		deps.CommandOutput = commandOutput
	}
	return &commandContext{deps: deps.withDefaults()}
}

func githubDoctorServer(t *testing.T, status int, body, scopes string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/user" {
			t.Fatalf("unexpected github probe: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Fatalf("missing bearer auth header: %q", got)
		}
		if scopes != "" {
			w.Header().Set("X-OAuth-Scopes", scopes)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

func findDoctorCheck(t *testing.T, checks []doctorCheck, name string) doctorCheck {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("doctor check %q not found in %+v", name, checks)
	return doctorCheck{}
}

func codexCanaryFake(t *testing.T, probeOutput string, probeErr error) func(context.Context, string, ...string) ([]byte, error) {
	t.Helper()
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch {
		case name == "/bin/git":
			return []byte("git version 2.43.0\n"), nil
		case name == "/bin/codex" && len(args) == 1 && args[0] == "--version":
			return []byte("codex-cli 0.136.0\n"), nil
		case name == "/bin/codex":
			return []byte(probeOutput), probeErr
		default:
			t.Fatalf("unexpected command: %s %v", name, args)
			return nil, nil
		}
	}
}

func TestDoctorCodexLaunchFlagsPass(t *testing.T) {
	setConfigEnv(t)
	c := doctorContext(t, map[string]string{"git": "/bin/git", "codex": "/bin/codex"}, codexCanaryFake(t, "ok\n", nil))

	check := findDoctorCheck(t, c.runDoctor(context.Background()), "codex-launch-flags")
	if check.Level != doctorPass || !strings.Contains(check.Message, "accepts") {
		t.Fatalf("canary = %+v, want PASS accepts", check)
	}
}

func TestDoctorCodexLaunchFlagsWarnOnRejectedFlag(t *testing.T) {
	setConfigEnv(t)
	c := doctorContext(t, map[string]string{"git": "/bin/git", "codex": "/bin/codex"},
		codexCanaryFake(t, "error: unexpected argument '--dangerously-bypass-hook-trust' found\n", errors.New("exit status 2")))

	check := findDoctorCheck(t, c.runDoctor(context.Background()), "codex-launch-flags")
	if check.Level != doctorWarn || !strings.Contains(check.Message, "rejected AO's launch flags") {
		t.Fatalf("canary = %+v, want WARN rejected flags", check)
	}
}

func TestDoctorCodexLaunchFlagsWarnOnUnknownConfigField(t *testing.T) {
	setConfigEnv(t)
	c := doctorContext(t, map[string]string{"git": "/bin/git", "codex": "/bin/codex"},
		codexCanaryFake(t, "unknown configuration field `hooks` in -c/--config override\n", nil))

	check := findDoctorCheck(t, c.runDoctor(context.Background()), "codex-launch-flags")
	if check.Level != doctorWarn || !strings.Contains(check.Message, "no longer recognizes") {
		t.Fatalf("canary = %+v, want WARN unknown config field", check)
	}
}

func TestDoctorCodexLaunchFlagsSkippedWithoutCodex(t *testing.T) {
	setConfigEnv(t)
	c := doctorContext(t, map[string]string{"git": "/bin/git"}, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("git version 2.43.0\n"), nil
	})

	check := findDoctorCheck(t, c.runDoctor(context.Background()), "codex-launch-flags")
	if check.Level != doctorPass || !strings.Contains(check.Message, "skipped") {
		t.Fatalf("canary = %+v, want skipped PASS", check)
	}
}

func TestDoctorHooksLogStates(t *testing.T) {
	gitOnly := func(context.Context, string, ...string) ([]byte, error) {
		return []byte("git version 2.43.0\n"), nil
	}

	t.Run("missing log passes", func(t *testing.T) {
		setConfigEnv(t)
		c := doctorContext(t, map[string]string{"git": "/bin/git"}, gitOnly)
		check := findDoctorCheck(t, c.runDoctor(context.Background()), "hooks-log")
		if check.Level != doctorPass || !strings.Contains(check.Message, "no hook delivery failures") {
			t.Fatalf("hooks-log = %+v, want PASS no failures", check)
		}
	})

	t.Run("recent failures warn", func(t *testing.T) {
		cfg := setConfigEnv(t)
		writeHooksLogLines(t, cfg.dataDir,
			time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339)+" session=old ao hooks codex stop: stale",
			time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)+" session=mer-1 ao hooks codex stop: connection refused",
		)
		c := doctorContext(t, map[string]string{"git": "/bin/git"}, gitOnly)
		check := findDoctorCheck(t, c.runDoctor(context.Background()), "hooks-log")
		if check.Level != doctorWarn || !strings.Contains(check.Message, "1 hook delivery failure") || !strings.Contains(check.Message, "connection refused") {
			t.Fatalf("hooks-log = %+v, want WARN with recent count and latest line", check)
		}
	})

	t.Run("only stale failures pass", func(t *testing.T) {
		cfg := setConfigEnv(t)
		writeHooksLogLines(t, cfg.dataDir,
			time.Now().Add(-72*time.Hour).UTC().Format(time.RFC3339)+" session=old ao hooks codex stop: stale",
		)
		c := doctorContext(t, map[string]string{"git": "/bin/git"}, gitOnly)
		check := findDoctorCheck(t, c.runDoctor(context.Background()), "hooks-log")
		if check.Level != doctorPass || !strings.Contains(check.Message, "last 24h") {
			t.Fatalf("hooks-log = %+v, want PASS stale-only", check)
		}
	})
}

func writeHooksLogLines(t *testing.T, dataDir string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dataDir, hooksLogName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
