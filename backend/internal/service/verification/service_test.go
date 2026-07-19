package verification

import (
	"context"
	"errors"
	"io"
	"os"
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
	return New(Deps{Store: store, Runner: runner, Policy: Policy{Profiles: map[string]Command{"unit": command}}, Auth: fakeAuthorizer{}, DataDir: t.TempDir()}), workspace
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

func TestRunDoesNotWriteWorkspaceState(t *testing.T) {
	svc, workspace := serviceFixture(t, Command{Argv: []string{"tool"}}, runnerFunc(func(context.Context, RunSpec) (RunResult, error) {
		return RunResult{}, nil
	}))
	res, err := svc.Run(context.Background(), "ao-1", "unit", "cap")
	if err != nil {
		t.Fatal(err)
	}
	if pathWithin(workspace, res.LogPath) {
		t.Fatalf("log path %q is inside workspace %q", res.LogPath, workspace)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".ao")); !os.IsNotExist(err) {
		t.Fatalf("workspace .ao was created: %v", err)
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
	if apiError.Message != "Verification capability is missing or invalid for this session" {
		t.Fatalf("message = %q", apiError.Message)
	}
}

func TestCapabilityIsCheckedBeforeSessionState(t *testing.T) {
	workspace := t.TempDir()
	store := fakeStore{
		session: domain.SessionRecord{ID: "ao-1", ProjectID: "ao", IsTerminated: true, Metadata: domain.SessionMetadata{WorkspacePath: workspace}},
		project: domain.ProjectRecord{ID: "ao"},
	}
	svc := New(Deps{Store: store, Runner: runnerFunc(func(context.Context, RunSpec) (RunResult, error) {
		t.Fatal("runner called")
		return RunResult{}, nil
	}), Auth: fakeAuthorizer{}, DataDir: t.TempDir()})
	_, err := svc.Run(context.Background(), "ao-1", "backend", "wrong")
	var apiError *apierr.Error
	if !errors.As(err, &apiError) || apiError.Code != "VERIFY_CAPABILITY_INVALID" {
		t.Fatalf("error = %v", err)
	}
}

func TestUnknownSessionDoesNotExposeExistenceWithoutCapability(t *testing.T) {
	svc := New(Deps{Store: fakeStore{}, Runner: runnerFunc(func(context.Context, RunSpec) (RunResult, error) {
		t.Fatal("runner called")
		return RunResult{}, nil
	}), Auth: fakeAuthorizer{}, DataDir: t.TempDir()})
	_, err := svc.Run(context.Background(), "enumerated-session", "backend", "wrong")
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
	}), Policy: Policy{Profiles: map[string]Command{"unit": {Argv: []string{"tool"}, WorkingDirectory: "linked"}}}, Auth: fakeAuthorizer{}, DataDir: t.TempDir()})
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
	dataDir := t.TempDir()
	var last string
	for range retainedLogs + 2 {
		f, path, err := newLogAt(dataDir, "ao-1")
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		last = path
	}
	entries, err := os.ReadDir(filepath.Dir(last))
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

func TestVerificationLogsAreUnderDataRoot(t *testing.T) {
	dataDir := t.TempDir()
	f, path, err := newLogAt(dataDir, "ao-1")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if !pathWithin(dataDir, path) {
		t.Fatalf("log %q escaped data root %q", path, dataDir)
	}
}

func TestNewLogCreatesFreshDataDirectoryHierarchy(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := os.Stat(filepath.Join(dataDir, "verification")); !os.IsNotExist(err) {
		t.Fatalf("verification hierarchy unexpectedly exists: %v", err)
	}
	f, path, err := newLogAt(dataDir, "ao-1")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("created log: %v", err)
	}
}

func TestNewLogSanitizesUntrustedSessionScope(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, path, err := newLogAt(dataDir, `../../outside\session`)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if !pathWithin(dataDir, path) {
		t.Fatalf("untrusted scope escaped data root: %q", path)
	}
	if _, err := os.Stat(filepath.Join(parent, "outside")); !os.IsNotExist(err) {
		t.Fatalf("scope created outside path: %v", err)
	}
}
