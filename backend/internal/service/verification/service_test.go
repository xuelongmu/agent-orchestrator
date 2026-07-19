package verification

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

type fakeStore struct {
	session domain.SessionRecord
	project domain.ProjectRecord
}

func (f fakeStore) GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error) {
	return f.session, f.session.ID != "", nil
}
func (f fakeStore) GetProject(context.Context, string) (domain.ProjectRecord, bool, error) {
	return f.project, f.project.ID != "", nil
}

type runnerFunc func(context.Context, RunSpec) (RunResult, error)

func (f runnerFunc) Run(ctx context.Context, spec RunSpec) (RunResult, error) { return f(ctx, spec) }

type fakeAuthorizer struct{}

func (fakeAuthorizer) Verify(session domain.SessionID, project domain.ProjectID, capability string) bool {
	return session == "ao-1" && project == "ao" && capability == "cap"
}

func serviceFixture(t *testing.T, command Command, runner Runner) (*Service, string) {
	t.Helper()
	workspace := t.TempDir()
	if command.WorkingDirectory != "" {
		if err := os.MkdirAll(filepath.Join(workspace, command.WorkingDirectory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store := fakeStore{
		session: domain.SessionRecord{ID: "ao-1", ProjectID: "ao", Metadata: domain.SessionMetadata{WorkspacePath: workspace}},
		project: domain.ProjectRecord{ID: "ao"},
	}
	return New(Deps{Store: store, Runner: runner, Policy: Policy{Profiles: map[string]Command{"unit": command}}, Auth: fakeAuthorizer{}}), workspace
}

func TestRunPreservesArgvAndReportsFailure(t *testing.T) {
	command := Command{Argv: []string{"tool", "argument with spaces", `quote"kept`}, WorkingDirectory: "src"}
	var got RunSpec
	svc, _ := serviceFixture(t, command, runnerFunc(func(_ context.Context, spec RunSpec) (RunResult, error) {
		got = spec
		_, _ = io.WriteString(spec.Output, "failed output\n")
		return RunResult{ExitCode: 7}, nil
	}))
	res, err := svc.Run(context.Background(), "ao-1", "unit", "cap")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Argv, "|") != strings.Join(command.Argv, "|") {
		t.Fatalf("argv = %#v, want %#v", got.Argv, command.Argv)
	}
	if res.Outcome != OutcomeFailed || res.ExitCode != 7 || res.LogPath == "" {
		t.Fatalf("result = %#v", res)
	}
	body, err := os.ReadFile(res.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "failed output") {
		t.Fatalf("log = %q", body)
	}
}

func TestRunCancellationAndTimeout(t *testing.T) {
	tests := []struct {
		name    string
		command Command
		setup   func() (context.Context, context.CancelFunc)
		want    Outcome
	}{
		{name: "canceled", command: Command{Argv: []string{"tool"}}, setup: func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }, want: OutcomeCanceled},
		{name: "timeout", command: Command{Argv: []string{"tool"}, TimeoutSeconds: 1}, setup: func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }, want: OutcomeTimedOut},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started := make(chan struct{})
			var once sync.Once
			svc, _ := serviceFixture(t, tt.command, runnerFunc(func(ctx context.Context, _ RunSpec) (RunResult, error) {
				once.Do(func() { close(started) })
				<-ctx.Done()
				return RunResult{ExitCode: -1}, ctx.Err()
			}))
			ctx, cancel := tt.setup()
			defer cancel()
			if tt.want == OutcomeCanceled {
				go func() { <-started; cancel() }()
			}
			res, err := svc.Run(ctx, "ao-1", "unit", "cap")
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != tt.want {
				t.Fatalf("outcome=%s want %s", res.Outcome, tt.want)
			}
		})
	}
}

func TestDaemonRootCancellationStopsRun(t *testing.T) {
	root, stop := context.WithCancel(context.Background())
	started := make(chan struct{})
	svc, _ := serviceFixture(t, Command{Argv: []string{"tool"}}, runnerFunc(func(ctx context.Context, _ RunSpec) (RunResult, error) {
		close(started)
		<-ctx.Done()
		return RunResult{ExitCode: -1}, ctx.Err()
	}))
	svc.root = root
	go func() { <-started; stop() }()
	res, err := svc.Run(context.Background(), "ao-1", "unit", "cap")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeCanceled {
		t.Fatalf("outcome = %s", res.Outcome)
	}
}

func TestRunRejectsConcurrentWorkspaceRun(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	svc, _ := serviceFixture(t, Command{Argv: []string{"tool"}}, runnerFunc(func(context.Context, RunSpec) (RunResult, error) { close(started); <-release; return RunResult{}, nil }))
	done := make(chan error, 1)
	go func() { _, err := svc.Run(context.Background(), "ao-1", "unit", "cap"); done <- err }()
	<-started
	_, err := svc.Run(context.Background(), "ao-1", "unit", "cap")
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != "VERIFY_ALREADY_RUNNING" {
		t.Fatalf("error=%v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunBoundsLogAndRetainsTail(t *testing.T) {
	payload := strings.Repeat("x", maxLogBytes) + "TAIL"
	svc, _ := serviceFixture(t, Command{Argv: []string{"tool"}}, runnerFunc(func(_ context.Context, s RunSpec) (RunResult, error) {
		_, err := io.WriteString(s.Output, payload)
		return RunResult{}, err
	}))
	res, err := svc.Run(context.Background(), "ao-1", "unit", "cap")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(res.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxLogBytes || !res.Truncated {
		t.Fatalf("size=%d truncated=%v", info.Size(), res.Truncated)
	}
	body, _ := os.ReadFile(res.LogPath)
	if !strings.HasSuffix(string(body), "TAIL") {
		t.Fatalf("log does not retain tail")
	}
}

func TestRunRejectsLogDirectorySymlink(t *testing.T) {
	outside := t.TempDir()
	svc, workspace := serviceFixture(t, Command{Argv: []string{"tool"}}, runnerFunc(func(context.Context, RunSpec) (RunResult, error) { return RunResult{}, nil }))
	if err := os.Symlink(outside, filepath.Join(workspace, ".ao")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := svc.Run(context.Background(), "ao-1", "unit", "cap")
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != "VERIFY_LOG_UNSAFE" {
		t.Fatalf("error=%v", err)
	}
}

func TestRunRejectsLogIgnoreSymlinkWithoutTouchingTarget(t *testing.T) {
	svc, workspace := serviceFixture(t, Command{Argv: []string{"tool"}}, runnerFunc(func(context.Context, RunSpec) (RunResult, error) {
		t.Fatal("runner called")
		return RunResult{}, nil
	}))
	if err := os.Mkdir(filepath.Join(workspace, ".ao"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(workspace, ".ao", ".gitignore")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := svc.Run(context.Background(), "ao-1", "unit", "cap")
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != "VERIFY_LOG_UNSAFE" {
		t.Fatalf("error=%v", err)
	}
	body, _ := os.ReadFile(target)
	if string(body) != "do not touch" {
		t.Fatalf("target was modified: %q", body)
	}
}

func TestUnknownProfileIsNotExecutableInput(t *testing.T) {
	svc, _ := serviceFixture(t, Command{Argv: []string{"tool"}}, runnerFunc(func(context.Context, RunSpec) (RunResult, error) { t.Fatal("runner called"); return RunResult{}, nil }))
	_, err := svc.Run(context.Background(), "ao-1", "rm -rf workspace", "cap")
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != "VERIFY_PROFILE_NOT_ALLOWED" {
		t.Fatalf("error=%v", err)
	}
}

func TestCapabilityCannotAuthorizeAnotherSession(t *testing.T) {
	svc, _ := serviceFixture(t, Command{Argv: []string{"tool"}}, runnerFunc(func(context.Context, RunSpec) (RunResult, error) {
		t.Fatal("runner called")
		return RunResult{}, nil
	}))
	_, err := svc.Run(context.Background(), "ao-1", "unit", "cap-from-another-session")
	var apiError *apierr.Error
	if !errors.As(err, &apiError) || apiError.Code != "VERIFY_CAPABILITY_INVALID" || apiError.Kind != apierr.KindForbidden {
		t.Fatalf("error = %v", err)
	}
}

func TestDefaultBackendAndFrontendProfiles(t *testing.T) {
	tests := map[string][]string{
		"backend":  {"go", "test", "./..."},
		"frontend": {"npm", "test", "--", "--run"},
	}
	for profile, want := range tests {
		t.Run(profile, func(t *testing.T) {
			got, ok := (Policy{}).withDefaults().Resolve("ao", profile)
			if !ok || strings.Join(got.Argv, "|") != strings.Join(want, "|") {
				t.Fatalf("Resolve() = %#v, %v", got, ok)
			}
		})
	}
}

func TestRunReportsStartFailureWithLog(t *testing.T) {
	svc, _ := serviceFixture(t, Command{Argv: []string{"missing"}}, runnerFunc(func(context.Context, RunSpec) (RunResult, error) {
		return RunResult{ExitCode: -1}, errors.New("executable not found")
	}))
	res, err := svc.Run(context.Background(), "ao-1", "unit", "cap")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeFailed || res.LogPath == "" || !strings.Contains(res.Error, "not found") {
		t.Fatalf("result = %#v", res)
	}
}

func TestWorkingDirectoryCannotEscapeThroughSymlink(t *testing.T) {
	outside := t.TempDir()
	workspace := t.TempDir()
	store := fakeStore{
		session: domain.SessionRecord{ID: "ao-1", ProjectID: "ao", Metadata: domain.SessionMetadata{WorkspacePath: workspace}},
		project: domain.ProjectRecord{ID: "ao"},
	}
	svc := New(Deps{Store: store, Runner: runnerFunc(func(context.Context, RunSpec) (RunResult, error) {
		t.Fatal("runner called")
		return RunResult{}, nil
	}), Policy: Policy{Profiles: map[string]Command{"unit": {Argv: []string{"tool"}, WorkingDirectory: "linked"}}}, Auth: fakeAuthorizer{}})
	if err := os.Symlink(outside, filepath.Join(workspace, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := svc.Run(context.Background(), "ao-1", "unit", "cap")
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != "VERIFY_WORKING_DIRECTORY_INVALID" {
		t.Fatalf("error = %v", err)
	}
}

func TestLogRetentionKeepsNewestTenAndAdvancesNumber(t *testing.T) {
	workspace := t.TempDir()
	var last string
	for range retainedLogs + 2 {
		f, path, err := newLog(workspace)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		last = path
	}
	entries, err := os.ReadDir(filepath.Join(workspace, ".ao"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if logNameRE.MatchString(entry.Name()) {
			count++
		}
	}
	if count != retainedLogs || !strings.HasSuffix(last, "verify-12.log") {
		t.Fatalf("logs = %d, last = %s", count, last)
	}
}

func TestVerificationLogsAreGitIgnored(t *testing.T) {
	workspace := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", workspace).CombinedOutput(); err != nil {
		t.Skipf("git unavailable: %v (%s)", err, out)
	}
	f, path, err := newLog(workspace)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	rel, _ := filepath.Rel(workspace, path)
	if out, err := exec.Command("git", "-C", workspace, "check-ignore", rel).CombinedOutput(); err != nil {
		t.Fatalf("log is not ignored: %v (%s)", err, out)
	}
}

