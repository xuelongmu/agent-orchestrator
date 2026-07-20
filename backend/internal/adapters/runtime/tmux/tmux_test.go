package tmux

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// -- fakeRunner test seam --

type fakeRunner struct {
	calls   []runnerCall
	outputs [][]byte
	err     error
}

type runnerCall struct {
	env  []string
	name string
	args []string
}

type runnerFunc func(context.Context, []string, string, ...string) ([]byte, error)

func (f runnerFunc) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	return f(ctx, env, name, args...)
}

func (f *fakeRunner) Run(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, runnerCall{env: append([]string(nil), env...), name: name, args: append([]string(nil), args...)})
	var out []byte
	if len(f.outputs) > 0 {
		out = f.outputs[0]
		f.outputs = f.outputs[1:]
	}
	if f.err != nil {
		return out, f.err
	}
	return out, nil
}

// recordingReaper replaces the production process-mutation boundary in unit
// tests. Destroy tests must never signal processes on the host running them.
type recordingReaper struct {
	anchoredPIDs []int
	calls        []reaperCall
}

type reaperCall struct {
	ctxCanceled bool
	anchors     []sessionAnchor
	grace       time.Duration
}

func (r *recordingReaper) Anchor(_ context.Context, panePIDs []int) []sessionAnchor {
	r.anchoredPIDs = append([]int(nil), panePIDs...)
	anchors := make([]sessionAnchor, 0, len(panePIDs))
	for _, pid := range panePIDs {
		identity := processIdentity{pid: pid, sessionID: pid, started: "anchored"}
		anchors = append(anchors, sessionAnchor{
			sessionID: pid,
			members:   map[processIdentity]struct{}{identity: {}},
		})
	}
	return anchors
}

func (r *recordingReaper) Reap(ctx context.Context, anchors []sessionAnchor, grace time.Duration) {
	r.calls = append(r.calls, reaperCall{
		ctxCanceled: ctx.Err() != nil,
		anchors:     append([]sessionAnchor(nil), anchors...),
		grace:       grace,
	})
}

// -- helpers --

func newTestRuntime(chunkSize int) (*Runtime, *fakeRunner) {
	fr := &fakeRunner{}
	r := New(Options{Binary: "tmux-test", Timeout: time.Second, Shell: "/bin/sh", ChunkSize: chunkSize})
	r.runner = fr
	r.enterDelay = 0 // tests must not pay the real 300ms pre-Enter pause
	r.sessionReaper = &recordingReaper{}
	return r, fr
}

// -- Options / New tests --

func TestNewDefaultsToPortableShell(t *testing.T) {
	t.Setenv("SHELL", "")
	r := New(Options{})
	if got := r.shell; got != "/bin/sh" {
		t.Fatalf("default shell = %q, want /bin/sh", got)
	}
}

func TestNewPicksUpShellFromEnv(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	r := New(Options{})
	if got := r.shell; got != "/bin/zsh" {
		t.Fatalf("shell = %q, want /bin/zsh", got)
	}
}

// -- command builder tests --

func TestCommandBuilders(t *testing.T) {
	if got, want := newSessionArgs("sess-1", "/tmp/ws", "/bin/sh", `echo hi; exec "${SHELL:-/bin/sh}" -i`),
		[]string{"new-session", "-d", "-s", "sess-1", "-x", "220", "-y", "50", "-c", "/tmp/ws", "/bin/sh", "-c", `echo hi; exec "${SHELL:-/bin/sh}" -i`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("newSessionArgs = %#v, want %#v", got, want)
	}
	// set-option uses pane-targeting (no = prefix).
	if got, want := setStatusOffArgs("sess-1"), []string{"set-option", "-t", "sess-1", "status", "off"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("setStatusOffArgs = %#v, want %#v", got, want)
	}
	if got, want := setWindowSizeLargestArgs("sess-1"), []string{"set-option", "-t", "sess-1", "window-size", "largest"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("setWindowSizeLargestArgs = %#v, want %#v", got, want)
	}
	if got, want := setMouseOnArgs("sess-1"), []string{"set-option", "-t", "sess-1", "mouse", "on"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("setMouseOnArgs = %#v, want %#v", got, want)
	}
	// kill-session and has-session use exact-match prefix =.
	if got, want := killSessionArgs("sess-1"), []string{"kill-session", "-t", "=sess-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("killSessionArgs = %#v, want %#v", got, want)
	}
	if got, want := hasSessionArgs("sess-1"), []string{"has-session", "-t", "=sess-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hasSessionArgs = %#v, want %#v", got, want)
	}
	if got, want := listPaneOwnersArgs("sess-1"), []string{"list-panes", "-s", "-t", "=sess-1", "-F", "#{pane_pid}\t#{window_linked}"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("listPaneOwnersArgs = %#v, want %#v", got, want)
	}
	if got, want := sendKeysLiteralArgs("sess-1", "hello"), []string{"send-keys", "-t", "sess-1", "-l", "hello"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sendKeysLiteralArgs = %#v, want %#v", got, want)
	}
	if got, want := sendEnterArgs("sess-1"), []string{"send-keys", "-t", "sess-1", "Enter"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sendEnterArgs = %#v, want %#v", got, want)
	}
	if got, want := sendInterruptArgs("sess-1"), []string{"send-keys", "-t", "sess-1", "C-c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sendInterruptArgs = %#v, want %#v", got, want)
	}
	if got, want := capturePaneArgs("sess-1", 10), []string{"capture-pane", "-t", "sess-1", "-p", "-S", "-10"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("capturePaneArgs = %#v, want %#v", got, want)
	}
}

// -- session name sanitization --

func TestSessionNameSanitizesSpecialChars(t *testing.T) {
	got, err := tmuxSessionName("repo/issue#42.1")
	if err != nil {
		t.Fatalf("tmuxSessionName: %v", err)
	}
	if !sessionIDPattern.MatchString(got) {
		t.Fatalf("sanitized id %q fails pattern", got)
	}
	if !strings.HasPrefix(got, "repo-issue-42-1-") {
		t.Fatalf("sanitized id = %q, want readable prefix", got)
	}
	if got == "repo/issue#42.1" {
		t.Fatal("sanitized id still contains raw unsafe characters")
	}
}

func TestSessionNamePassesThroughShortConforming(t *testing.T) {
	if got := SessionName("myproj-1"); got != "myproj-1" {
		t.Fatalf("SessionName = %q, want unchanged", got)
	}
}

func TestSessionNameMatchesCreateNaming(t *testing.T) {
	long := domain.SessionID(strings.Repeat("x", 60) + "-1")
	viaCreate, err := tmuxSessionName(long)
	if err != nil {
		t.Fatalf("tmuxSessionName: %v", err)
	}
	if got := SessionName(string(long)); got != viaCreate {
		t.Fatalf("SessionName = %q, but Create uses %q", got, viaCreate)
	}
	if SessionName(string(long)) == string(long) {
		t.Fatal("expected long id to be sanitised to a different name")
	}
}

// -- env key validation --

func TestCreateRejectsInvalidEnvKeys(t *testing.T) {
	r, fr := newTestRuntime(0)
	_ = fr
	_, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID:     "sess-1",
		WorkspacePath: "/tmp/ws",
		Argv:          []string{"echo", "hi"},
		Env:           map[string]string{"BAD KEY": "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid env key") {
		t.Fatalf("Create err = %v, want invalid env key", err)
	}
}

// -- Create tests --

func TestCreateIssuesNewSessionAndStatusOff(t *testing.T) {
	// new-session, set-option status, set-option mouse, set-option window-size,
	// has-session (exit 0 = alive)
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{nil, nil, nil, nil, nil}

	h, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID:     "sess-1",
		WorkspacePath: "/tmp/ws",
		Argv:          []string{"echo", "hi"},
		Env:           map[string]string{"AO_SESSION_ID": "sess-1"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.ID != "sess-1" {
		t.Fatalf("handle ID = %q, want sess-1", h.ID)
	}
	// Expect 5 calls: new-session, set-option status, set-option mouse,
	// set-option window-size, has-session.
	if len(fr.calls) != 5 {
		t.Fatalf("calls = %d, want 5", len(fr.calls))
	}

	// Call 0: new-session
	if got := fr.calls[0].args[0]; got != "new-session" {
		t.Fatalf("call[0] = %q, want new-session", got)
	}
	// Check -s <id>, -c <cwd> are present.
	joined := strings.Join(fr.calls[0].args, " ")
	if !strings.Contains(joined, "-s sess-1") {
		t.Fatalf("new-session args missing -s sess-1: %v", fr.calls[0].args)
	}
	if !strings.Contains(joined, "-c /tmp/ws") {
		t.Fatalf("new-session args missing -c /tmp/ws: %v", fr.calls[0].args)
	}
	// Ensure -x and -y are set.
	if !strings.Contains(joined, "-x 220") || !strings.Contains(joined, "-y 50") {
		t.Fatalf("new-session args missing -x/-y: %v", fr.calls[0].args)
	}

	// Call 1: set-option status off (plain target, pane-targeting does not use =).
	if got, want := fr.calls[1].args, setStatusOffArgs("sess-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("call[1] = %#v, want %#v", got, want)
	}

	// Call 2: set-option mouse on (enables wheel-scroll of the pane).
	if got, want := fr.calls[2].args, setMouseOnArgs("sess-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("call[2] = %#v, want %#v", got, want)
	}

	// Call 3: set-option window-size largest (multi-client sizing, see
	// setWindowSizeLargestArgs).
	if got, want := fr.calls[3].args, setWindowSizeLargestArgs("sess-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("call[3] = %#v, want %#v", got, want)
	}

	// Call 4: has-session (IsAlive, uses exact-match target =sess-1).
	if got, want := fr.calls[4].args, hasSessionArgs("sess-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("call[4] = %#v, want %#v", got, want)
	}
}

func TestCreateLaunchCommandContainsKeepAliveShell(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{nil, nil, nil}

	_, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID:     "sess-1",
		WorkspacePath: "/tmp/ws",
		Argv:          []string{"myagent", "--flag"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The launch command is the last argument to new-session (after shellPath -c).
	args := fr.calls[0].args
	launchCmd := args[len(args)-1]
	if !strings.Contains(launchCmd, `exec "${SHELL:-/bin/sh}" -i`) {
		t.Fatalf("launch command missing keep-alive shell: %q", launchCmd)
	}
	if !strings.Contains(launchCmd, "'myagent'") {
		t.Fatalf("launch command missing quoted argv: %q", launchCmd)
	}
}

func TestCreateLaunchCommandExportsEnvVars(t *testing.T) {
	oldGetenv := getenv
	getenv = func(key string) string {
		if key == "PATH" {
			return "/usr/bin:/bin"
		}
		return ""
	}
	defer func() { getenv = oldGetenv }()

	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{nil, nil, nil}

	_, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID:     "sess-1",
		WorkspacePath: "/tmp/ws",
		Argv:          []string{"myagent"},
		Env: map[string]string{
			"AO_SESSION_ID": "sess-1",
			"ODD":           "can't",
			"PATH":          "/custom/bin:/usr/bin",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	args := fr.calls[0].args
	launchCmd := args[len(args)-1]
	for _, want := range []string{
		"export AO_SESSION_ID='sess-1';",
		"export ODD='can'\\''t';",
		"export PATH='/custom/bin:/usr/bin';",
	} {
		if !strings.Contains(launchCmd, want) {
			t.Fatalf("launch command missing %q in: %q", want, launchCmd)
		}
	}
}

func TestCreateDestroysAndReturnsErrorWhenNotAlive(t *testing.T) {
	// Every setup command succeeds; only the has-session liveness probe reports the
	// session as gone, so Create must fail on the liveness check specifically.
	r2, _ := newTestRuntime(0)
	fr3 := &fakeRunnerSelectiveErr{
		exitErrOn: "has-session",
		errOutput: []byte("can't find session: sess-1"),
	}
	r2.runner = fr3

	_, err := r2.Create(context.Background(), ports.RuntimeConfig{
		SessionID:     "sess-1",
		WorkspacePath: "/tmp/ws",
		Argv:          []string{"myagent"},
	})
	if err == nil {
		t.Fatal("Create: got nil, want error when session not alive after create")
	}
	// The failure must come from the liveness probe, not from an earlier setup
	// command. Without this the test would still pass if a newly inserted tmux
	// call took the injected error first — which is exactly what happened once.
	if !strings.Contains(err.Error(), "exited before ready") {
		t.Fatalf("Create err = %v, want the liveness-check failure (exited before ready)", err)
	}
	sawHasSession := false
	for _, c := range fr3.calls {
		if len(c.args) > 0 && c.args[0] == "has-session" {
			sawHasSession = true
		}
	}
	if !sawHasSession {
		t.Fatal("Create never reached the has-session liveness probe")
	}
	// Verify Destroy was called (kill-session).
	hasKill := false
	for _, c := range fr3.calls {
		if len(c.args) > 0 && c.args[0] == "kill-session" {
			hasKill = true
		}
	}
	if !hasKill {
		t.Fatal("expected kill-session cleanup call when session not alive")
	}
}

// fakeRunnerSelectiveErr returns an exec.ExitError (carrying errOutput) for the
// call whose tmux subcommand is exitErrOn, and succeeds for every other call.
// Matching on the subcommand rather than a call index is deliberate: Create's
// command sequence grows over time, and an index would silently retarget the
// injected failure onto whichever command was inserted before the intended one.
type fakeRunnerSelectiveErr struct {
	calls     []runnerCall
	exitErrOn string
	errOutput []byte
	outputs   map[string][]byte
}

func (f *fakeRunnerSelectiveErr) Run(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, runnerCall{env: append([]string(nil), env...), name: name, args: append([]string(nil), args...)})
	if len(args) > 0 && args[0] == f.exitErrOn {
		return f.errOutput, &exec.ExitError{}
	}
	if len(args) > 0 {
		return f.outputs[args[0]], nil
	}
	return nil, nil
}

// -- Destroy tests --

func TestDestroyIsIdempotentWhenSessionMissing(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{nil, []byte("can't find session: sess-1")}
	fr.err = &exec.ExitError{}

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(fr.calls) != 2 || fr.calls[0].args[0] != "list-panes" || fr.calls[1].args[0] != "kill-session" {
		t.Fatalf("calls = %#v, want list-panes then kill-session", fr.calls)
	}
}

func TestDestroyIsIdempotentWhenNoServer(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{nil, []byte("no server running on /tmp/tmux-1000/default")}
	fr.err = &exec.ExitError{}

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy no-server: %v", err)
	}
}

func TestDestroyReportsUnexpectedFailures(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{nil, []byte("permission denied")}
	fr.err = &exec.ExitError{}

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err == nil {
		t.Fatal("Destroy: got nil, want unexpected failure error")
	}
}

func TestDestroyArgs(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{nil, nil}

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if got, want := fr.calls[0].args, listPaneOwnersArgs("sess-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("list pane args = %#v, want %#v", got, want)
	}
	// killSessionArgs uses exact-match target =<id>.
	if got, want := fr.calls[1].args, killSessionArgs("sess-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("destroy args = %#v, want %#v", got, want)
	}
}

func TestDestroyReapsAllDiscoveredPaneSessions(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{[]byte("4242 0\n4243 0\n1 0\n0 0\n4242 0\nnoise\n"), nil}
	reaper := &recordingReaper{}
	r.sessionReaper = reaper

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(reaper.calls) != 1 {
		t.Fatalf("reaper calls = %d, want 1", len(reaper.calls))
	}
	if got, want := reaper.anchoredPIDs, []int{4242, 4243}; !reflect.DeepEqual(got, want) {
		t.Fatalf("anchored pane pids = %#v, want %#v", got, want)
	}
	if got, want := anchorSessionIDs(reaper.calls[0].anchors), []int{4242, 4243}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reaped session ids = %#v, want %#v", got, want)
	}
	if reaper.calls[0].grace != r.reapGrace {
		t.Fatalf("reap grace = %v, want %v", reaper.calls[0].grace, r.reapGrace)
	}
}

func TestDestroyExcludesPanesFromLinkedWindows(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{[]byte("4242 0\n5000 1\n5001 1\n4243 0\n"), nil}
	reaper := &recordingReaper{}
	r.sessionReaper = reaper

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if got, want := reaper.anchoredPIDs, []int{4242, 4243}; !reflect.DeepEqual(got, want) {
		t.Fatalf("anchored pane pids = %#v, want only unlinked panes %#v", got, want)
	}
}

func TestDestroyStillReapsWhenKillSessionFails(t *testing.T) {
	r, _ := newTestRuntime(0)
	fr := &fakeRunnerSelectiveErr{
		exitErrOn: "kill-session",
		errOutput: []byte("permission denied"),
		outputs: map[string][]byte{
			"list-panes": []byte("4242 0\n"),
		},
	}
	r.runner = fr
	reaper := &recordingReaper{}
	r.sessionReaper = reaper

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err == nil {
		t.Fatal("Destroy: got nil, want kill-session error")
	}
	if got, want := anchorSessionIDs(reaper.calls[0].anchors), []int{4242}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reaped session ids = %#v, want %#v", got, want)
	}
}

func anchorSessionIDs(anchors []sessionAnchor) []int {
	ids := make([]int, 0, len(anchors))
	for _, anchor := range anchors {
		ids = append(ids, anchor.sessionID)
	}
	return ids
}

func TestDestroyMissingTmuxIsNotTreatedAsMissingSession(t *testing.T) {
	r, _ := newTestRuntime(0)
	r.runner = runnerFunc(func(_ context.Context, _ []string, _ string, _ ...string) ([]byte, error) {
		return nil, exec.ErrNotFound
	})

	err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"})
	if err == nil || !strings.Contains(err.Error(), "executable file not found") {
		t.Fatalf("Destroy err = %v, want missing tmux error", err)
	}
}

func TestDestroyReservesCallerDeadlineForKillSession(t *testing.T) {
	r, _ := newTestRuntime(0)
	r.timeout = 50 * time.Millisecond
	reaper := &recordingReaper{}
	r.sessionReaper = reaper
	r.runner = runnerFunc(func(ctx context.Context, _ []string, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "list-panes":
			<-ctx.Done()
			return nil, ctx.Err()
		case "kill-session":
			if ctx.Err() != nil {
				t.Fatalf("kill-session received exhausted caller context: %v", ctx.Err())
			}
			return nil, nil
		default:
			t.Fatalf("unexpected tmux command %q", args[0])
			return nil, nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Millisecond)
	defer cancel()

	if err := r.Destroy(ctx, ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}

type recordingSignaler struct {
	calls []signalCall
	errs  map[int]error
}

type signalCall struct {
	pid    int
	signal os.Signal
}

func (s *recordingSignaler) Signal(pid int, signal os.Signal) error {
	s.calls = append(s.calls, signalCall{pid: pid, signal: signal})
	return s.errs[pid]
}

type processSnapshotStep struct {
	processes []processIdentity
	current   map[int]processIdentity
	err       error
}

type fakeProcessTable struct {
	current       map[int]processIdentity
	snapshots     []processSnapshotStep
	identityCalls []int
}

func (f *fakeProcessTable) Identity(ctx context.Context, pid int) (processIdentity, error) {
	if err := ctx.Err(); err != nil {
		return processIdentity{}, err
	}
	f.identityCalls = append(f.identityCalls, pid)
	identity, ok := f.current[pid]
	if !ok {
		return processIdentity{}, errors.New("process missing")
	}
	return identity, nil
}

func (f *fakeProcessTable) Snapshot(ctx context.Context, _ map[int]struct{}) ([]processIdentity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(f.snapshots) == 0 {
		return nil, errors.New("unexpected process snapshot")
	}
	step := f.snapshots[0]
	f.snapshots = f.snapshots[1:]
	if step.current != nil {
		f.current = step.current
	}
	return append([]processIdentity(nil), step.processes...), step.err
}

func identity(pid, sid int, started string) processIdentity {
	return processIdentity{pid: pid, sessionID: sid, started: started}
}

func anchor(sid int, members ...processIdentity) sessionAnchor {
	set := make(map[processIdentity]struct{}, len(members))
	for _, member := range members {
		set[member] = struct{}{}
	}
	return sessionAnchor{sessionID: sid, members: set}
}

func newTestProcessReaper(table processTable, signaler processSignaler) processSessionReaper {
	return processSessionReaper{
		table:    table,
		signaler: signaler,
		timeout:  time.Second,
		wait: func(ctx context.Context, _ time.Duration) bool {
			return ctx.Err() == nil
		},
	}
}

func TestProcessSessionReaperAnchorsOwnedSessionBeforeTeardown(t *testing.T) {
	leader := identity(4242, 4242, "leader-start")
	child := identity(5000, 4242, "child-start")
	notLeader := identity(6000, 4242, "not-a-session-leader")
	table := &fakeProcessTable{
		current: map[int]processIdentity{4242: leader, 6000: notLeader},
		snapshots: []processSnapshotStep{{
			processes: []processIdentity{leader, child},
		}},
	}
	reaper := newTestProcessReaper(table, &recordingSignaler{})

	anchors := reaper.Anchor(context.Background(), []int{0, 1, 4242, 4242, 6000})
	if got, want := anchorSessionIDs(anchors), []int{4242}; !reflect.DeepEqual(got, want) {
		t.Fatalf("anchor session ids = %#v, want %#v", got, want)
	}
	if _, ok := anchors[0].members[child]; !ok {
		t.Fatalf("anchor members = %#v, want pre-teardown child %#v", anchors[0].members, child)
	}
	if got, want := table.identityCalls, []int{4242, 6000, 4242}; !reflect.DeepEqual(got, want) {
		t.Fatalf("identity calls = %#v, want leader discovery and immediate revalidation %#v", got, want)
	}
}

func TestProcessSessionReaperRevalidatesBeforeEverySignal(t *testing.T) {
	process := identity(5000, 4242, "original")
	reused := identity(5000, 9999, "reused")
	table := &fakeProcessTable{
		current: map[int]processIdentity{5000: process},
		snapshots: []processSnapshotStep{
			{processes: []processIdentity{process}},
			{processes: []processIdentity{process}},
			{processes: []processIdentity{process}},
			{processes: []processIdentity{process}},
			// Snapshot saw the old identity, but the immediate pre-KILL check
			// sees a reused PID in another session.
			{processes: []processIdentity{process}, current: map[int]processIdentity{5000: reused}},
		},
	}
	signaler := &recordingSignaler{}
	reaper := newTestProcessReaper(table, signaler)
	reaper.Reap(context.Background(), []sessionAnchor{anchor(4242, process)}, 4*time.Second)

	if got, want := signaler.calls, []signalCall{{pid: 5000, signal: syscall.SIGTERM}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("signals = %#v, want no KILL after immediate identity mismatch %#v", got, want)
	}
	if got := len(table.identityCalls); got < 6 {
		t.Fatalf("identity checks = %d, want checks across snapshots and before each signal", got)
	}
}

func TestProcessSessionReaperResnapshotsAndReapsLateChild(t *testing.T) {
	original := identity(5000, 4242, "original")
	late := identity(5001, 4242, "late")
	both := map[int]processIdentity{5000: original, 5001: late}
	table := &fakeProcessTable{
		current: map[int]processIdentity{5000: original},
		snapshots: []processSnapshotStep{
			{processes: []processIdentity{original}},
			{processes: []processIdentity{original, late}, current: both},
			{processes: []processIdentity{original, late}},
			{processes: []processIdentity{original, late}},
			{processes: []processIdentity{original, late}},
		},
	}
	signaler := &recordingSignaler{}
	waits := 0
	reaper := newTestProcessReaper(table, signaler)
	reaper.wait = func(ctx context.Context, duration time.Duration) bool {
		waits++
		if ctx.Err() != nil {
			t.Fatalf("wait received canceled context: %v", ctx.Err())
		}
		if duration != time.Second {
			t.Fatalf("grace slice = %v, want 1s", duration)
		}
		return true
	}

	reaper.Reap(context.Background(), []sessionAnchor{anchor(4242, original)}, 4*time.Second)
	if waits != reapGraceSlices {
		t.Fatalf("grace waits = %d, want %d bounded resnapshots", waits, reapGraceSlices)
	}
	want := []signalCall{
		{pid: 5000, signal: syscall.SIGTERM},
		{pid: 5001, signal: syscall.SIGTERM},
		{pid: 5000, signal: syscall.SIGKILL},
		{pid: 5001, signal: syscall.SIGKILL},
	}
	if !reflect.DeepEqual(signaler.calls, want) {
		t.Fatalf("signals = %#v, want late child TERM and final KILL %#v", signaler.calls, want)
	}
}

func TestProcessSessionReaperStopsWhenSessionContinuityIsLost(t *testing.T) {
	original := identity(5000, 4242, "original")
	unrelated := identity(6000, 4242, "unanchored")
	table := &fakeProcessTable{
		current:   map[int]processIdentity{6000: unrelated},
		snapshots: []processSnapshotStep{{processes: []processIdentity{unrelated}}},
	}
	signaler := &recordingSignaler{}
	reaper := newTestProcessReaper(table, signaler)
	reaper.Reap(context.Background(), []sessionAnchor{anchor(4242, original)}, 5*time.Second)
	if len(signaler.calls) != 0 {
		t.Fatalf("signals = %#v, want none without an anchored continuity witness", signaler.calls)
	}
}

func TestProcessSessionReaperFinishesBoundedCleanupAfterCallerCancellation(t *testing.T) {
	process := identity(5000, 4242, "original")
	table := &fakeProcessTable{
		current: map[int]processIdentity{5000: process},
		snapshots: []processSnapshotStep{
			{processes: []processIdentity{process}},
			{processes: []processIdentity{process}},
			{processes: []processIdentity{process}},
			{processes: []processIdentity{process}},
			{processes: []processIdentity{process}},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	signaler := &recordingSignaler{}
	reaper := newTestProcessReaper(table, signaler)
	reaper.Reap(ctx, []sessionAnchor{anchor(4242, process)}, 4*time.Second)
	want := []signalCall{{pid: 5000, signal: syscall.SIGTERM}, {pid: 5000, signal: syscall.SIGKILL}}
	if !reflect.DeepEqual(signaler.calls, want) {
		t.Fatalf("signals = %#v, want bounded detached cleanup %#v", signaler.calls, want)
	}
}

// -- IsAlive tests --

func TestIsAliveReturnsTrueOnExitZero(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{nil}

	alive, err := r.IsAlive(context.Background(), ports.RuntimeHandle{ID: "sess-1"})
	if err != nil {
		t.Fatalf("IsAlive: %v", err)
	}
	if !alive {
		t.Fatal("alive = false, want true")
	}
	if got, want := fr.calls[0].args, hasSessionArgs("sess-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("has-session args = %#v, want %#v", got, want)
	}
}

func TestIsAliveReturnsFalseNilOnCantFindSession(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{[]byte("can't find session: sess-1")}
	fr.err = &exec.ExitError{}

	alive, err := r.IsAlive(context.Background(), ports.RuntimeHandle{ID: "sess-1"})
	if err != nil {
		t.Fatalf("IsAlive: %v", err)
	}
	if alive {
		t.Fatal("alive = true, want false")
	}
}

func TestIsAliveReturnsFalseNilOnNoServer(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{[]byte("no server running on /tmp/tmux-1000/default")}
	fr.err = &exec.ExitError{}

	alive, err := r.IsAlive(context.Background(), ports.RuntimeHandle{ID: "sess-1"})
	if err != nil {
		t.Fatalf("IsAlive: %v", err)
	}
	if alive {
		t.Fatal("alive = true, want false")
	}
}

func TestIsAliveReturnsFalseNilOnErrorConnecting(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{[]byte("error connecting to /tmp/tmux-1000/default (No such file or directory)")}
	fr.err = &exec.ExitError{}

	alive, err := r.IsAlive(context.Background(), ports.RuntimeHandle{ID: "sess-1"})
	if err != nil {
		t.Fatalf("IsAlive error connecting: %v", err)
	}
	if alive {
		t.Fatal("alive = true, want false")
	}
}

// IsAlive must treat any non-"missing" non-zero exit as a probe error so the
// reaper never reads a transient failure as proof of death.
func TestIsAliveReportsOtherExitFailuresAsProbeErrors(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{[]byte("unexpected internal error")}
	fr.err = &exec.ExitError{}

	alive, err := r.IsAlive(context.Background(), ports.RuntimeHandle{ID: "sess-1"})
	if err == nil {
		t.Fatal("IsAlive: got nil, want probe error; failed probe must not read as dead")
	}
	if alive {
		t.Fatal("alive = true on probe failure")
	}
}

// -- SendMessage tests --

func TestSendMessageChunksAndSendsEnter(t *testing.T) {
	r, fr := newTestRuntime(5) // chunkSize=5
	// "hello世界": hello=5 bytes, 世=3 bytes, 界=3 bytes => 3 sends + 1 Enter
	if err := r.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, "hello世界"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if len(fr.calls) != 4 {
		t.Fatalf("calls = %d, want 4 (3 chunks + Enter)", len(fr.calls))
	}
	if got, want := fr.calls[0].args, sendKeysLiteralArgs("sess-1", "hello"); !reflect.DeepEqual(got, want) {
		t.Fatalf("chunk 1 args = %#v, want %#v", got, want)
	}
	if got, want := fr.calls[1].args, sendKeysLiteralArgs("sess-1", "世"); !reflect.DeepEqual(got, want) {
		t.Fatalf("chunk 2 args = %#v, want %#v", got, want)
	}
	if got, want := fr.calls[2].args, sendKeysLiteralArgs("sess-1", "界"); !reflect.DeepEqual(got, want) {
		t.Fatalf("chunk 3 args = %#v, want %#v", got, want)
	}
	if got, want := fr.calls[3].args, sendEnterArgs("sess-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Enter args = %#v, want %#v", got, want)
	}
}

func TestSendMessageUsesLiteralFlag(t *testing.T) {
	r, fr := newTestRuntime(0)
	if err := r.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, "Enter"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	// First call must use -l so "Enter" is sent literally, not as a key binding.
	if fr.calls[0].args[3] != "-l" {
		t.Fatalf("send-keys args[3] = %q, want -l", fr.calls[0].args[3])
	}
}

// TestSendMessageDelaysBeforeEnter verifies the pre-Enter pause (mirroring
// conpty's ptyInputEnterDelay) fires only for a non-empty message: a large
// multiline paste needs time to settle before the trailing Enter, or the Enter
// is absorbed and the prompt is left unsubmitted (issue #2342). An empty
// (nudge) message skips the pause — there is no paste ahead of a catch-up Enter.
func TestSendMessageDelaysBeforeEnter(t *testing.T) {
	// enterDelay=0 (the test default) => no pause: SendMessage is near-instant.
	r0, _ := newTestRuntime(0)
	r0.enterDelay = 0
	start := time.Now()
	if err := r0.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, "hi"); err != nil {
		t.Fatalf("SendMessage (no delay): %v", err)
	}
	if dt := time.Since(start); dt > 50*time.Millisecond {
		t.Fatalf("SendMessage with enterDelay=0 took %s; want no real pause", dt)
	}

	// enterDelay>0 => SendMessage blocks at least enterDelay before Enter, but
	// only for a non-empty message.
	r, fr := newTestRuntime(0)
	r.enterDelay = 30 * time.Millisecond
	start = time.Now()
	if err := r.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, "hello"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if dt := time.Since(start); dt < r.enterDelay {
		t.Fatalf("SendMessage took %s, want >= %s pre-Enter pause", dt, r.enterDelay)
	}
	// Non-empty message still ends with the literal chunks then Enter.
	if len(fr.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (chunk + Enter)", len(fr.calls))
	}
	if got, want := fr.calls[1].args, sendEnterArgs("sess-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Enter args = %#v, want %#v", got, want)
	}

	// Empty (nudge) message: no paste, no pause — even with enterDelay set.
	rNudge, frNudge := newTestRuntime(0)
	rNudge.enterDelay = 30 * time.Millisecond
	start = time.Now()
	if err := rNudge.SendMessage(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, ""); err != nil {
		t.Fatalf("SendMessage (nudge): %v", err)
	}
	if dt := time.Since(start); dt > 50*time.Millisecond {
		t.Fatalf("nudge SendMessage took %s; want no pause for empty message", dt)
	}
	// Empty message is Enter-only: no send-keys -l call, just Enter.
	if len(frNudge.calls) != 1 {
		t.Fatalf("nudge calls = %d, want 1 (Enter only)", len(frNudge.calls))
	}
	if got, want := frNudge.calls[0].args, sendEnterArgs("sess-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("nudge Enter args = %#v, want %#v", got, want)
	}
}

// TestSendMessageEnterSurvivesCallerCancel pins the detached-Enter contract:
// once the chunks are pasted, a caller cancellation landing in the pre-Enter
// pause must NOT abandon the send — the pasted draft would sit unsubmitted and
// a retried send would double-paste. The pause and Enter run on a context
// detached from the caller's, so SendMessage completes (chunks then Enter).
func TestSendMessageEnterSurvivesCallerCancel(t *testing.T) {
	r, fr := newTestRuntime(0)
	// A pause long enough that the 50ms-delayed cancel deterministically lands
	// inside it (the chunk send is near-instant against the fake runner).
	r.enterDelay = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()

	if err := r.SendMessage(ctx, ports.RuntimeHandle{ID: "sess-1"}, "hello"); err != nil {
		t.Fatalf("SendMessage cancelled mid-pause: %v (Enter must run detached)", err)
	}
	if len(fr.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (chunk + Enter despite the caller cancel after the paste)", len(fr.calls))
	}
	if got, want := fr.calls[1].args, sendEnterArgs("sess-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Enter args = %#v, want %#v", got, want)
	}
}

func TestInterruptSendsCtrlC(t *testing.T) {
	r, fr := newTestRuntime(0)
	if err := r.Interrupt(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if got, want := fr.calls[0].args, sendInterruptArgs("sess-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("interrupt args = %#v, want %#v", got, want)
	}
}

// -- GetOutput tests --

func TestGetOutputValidatesLines(t *testing.T) {
	r, _ := newTestRuntime(0)
	_, err := r.GetOutput(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, 0)
	if err == nil {
		t.Fatal("GetOutput lines=0: got nil, want error")
	}
}

func TestGetOutputTrimsLines(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{[]byte("one\ntwo\nthree\n")}

	out, err := r.GetOutput(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, 2)
	if err != nil {
		t.Fatalf("GetOutput: %v", err)
	}
	if out != "two\nthree\n" {
		t.Fatalf("output = %q, want last two lines", out)
	}
}

func TestGetOutputTrimsTrailingScreenPaddingBeforeTailing(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{[]byte("ready\nprompt> echo hi\nhi\n\n\n\n")}

	out, err := r.GetOutput(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, 2)
	if err != nil {
		t.Fatalf("GetOutput: %v", err)
	}
	if out != "prompt> echo hi\nhi\n" {
		t.Fatalf("output = %q, want last non-padding lines", out)
	}
}

func TestGetOutputArgs(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{[]byte("output\n")}

	_, err := r.GetOutput(context.Background(), ports.RuntimeHandle{ID: "sess-1"}, 10)
	if err != nil {
		t.Fatalf("GetOutput: %v", err)
	}
	if got, want := fr.calls[0].args, capturePaneArgs("sess-1", 10); !reflect.DeepEqual(got, want) {
		t.Fatalf("capture-pane args = %#v, want %#v", got, want)
	}
}

// -- AttachCommand tests --

func TestAttachCommandReturnsExpectedArgv(t *testing.T) {
	r := New(Options{Binary: "/usr/bin/tmux", Timeout: time.Second})
	argv, err := r.attachCommand(ports.RuntimeHandle{ID: "sess-1"})
	if err != nil {
		t.Fatalf("AttachCommand: %v", err)
	}
	want := []string{"/usr/bin/tmux", "-u", "attach-session", "-t", "sess-1"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv = %#v, want %#v", argv, want)
	}
}

func TestAttachCommandRejectsInvalidHandle(t *testing.T) {
	r := New(Options{})
	_, err := r.attachCommand(ports.RuntimeHandle{ID: ""})
	if err == nil {
		t.Fatal("AttachCommand empty handle: got nil, want error")
	}
}

func TestAttachEnvForcesUsableTerm(t *testing.T) {
	env := attachEnv([]string{"PATH=/bin", "TERM=dumb", "SHELL=/bin/sh"})
	if got, want := env, []string{"PATH=/bin", "TERM=xterm-256color", "SHELL=/bin/sh"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("attachEnv = %#v, want %#v", got, want)
	}

	env = attachEnv([]string{"PATH=/bin"})
	if got, want := env, []string{"PATH=/bin", "TERM=xterm-256color"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("attachEnv without TERM = %#v, want %#v", got, want)
	}
}

// -- commandError tests --

func TestCommandErrorUnwraps(t *testing.T) {
	base := errors.New("base")
	err := commandError{err: base, output: "details"}
	if !errors.Is(err, base) {
		t.Fatal("commandError should unwrap base error")
	}
	if !strings.Contains(err.Error(), "details") {
		t.Fatalf("error = %q, want output details", err.Error())
	}
}

// -- text helper tests --

func TestChunks(t *testing.T) {
	if got := chunks("", 5); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("chunks empty = %#v", got)
	}
	if got := chunks("hello", 10); !reflect.DeepEqual(got, []string{"hello"}) {
		t.Fatalf("chunks fits = %#v", got)
	}
	// UTF-8 boundary: 世 is 3 bytes; with chunkSize=5 "hello世界" splits at 5,6,6
	got := chunks("hello世界", 5)
	if len(got) != 3 {
		t.Fatalf("chunks count = %d, want 3: %#v", len(got), got)
	}
	if got[0] != "hello" || got[1] != "世" || got[2] != "界" {
		t.Fatalf("chunks = %#v, want [hello 世 界]", got)
	}
}

func TestTailLines(t *testing.T) {
	if got := tailLines("a\nb\nc\n", 2); got != "b\nc\n" {
		t.Fatalf("tailLines = %q, want b/c", got)
	}
	if got := tailLines("a\nb\n", 5); got != "a\nb\n" {
		t.Fatalf("tailLines fewer = %q", got)
	}
	if got := tailLines("", 5); got != "" {
		t.Fatalf("tailLines empty = %q", got)
	}
}

func TestTrimTrailingBlankLines(t *testing.T) {
	if got := trimTrailingBlankLines("a\nb\n\n\n"); got != "a\nb\n" {
		t.Fatalf("trimTrailingBlankLines = %q, want a/b", got)
	}
	if got := trimTrailingBlankLines(""); got != "" {
		t.Fatalf("trimTrailingBlankLines empty = %q", got)
	}
}
