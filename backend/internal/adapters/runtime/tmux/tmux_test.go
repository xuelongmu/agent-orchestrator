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
	anchoredPanes []paneRef
	calls         []reaperCall
	serverExited  bool
	serverExitErr error
	anchorHook    func(context.Context)
}

type reaperCall struct {
	ctxCanceled bool
	anchors     []sessionAnchor
	grace       time.Duration
}

func (r *recordingReaper) Anchor(ctx context.Context, panes []paneRef) []sessionAnchor {
	if r.anchorHook != nil {
		r.anchorHook(ctx)
	}
	r.anchoredPanes = append([]paneRef(nil), panes...)
	anchors := make([]sessionAnchor, 0, len(panes))
	for _, pane := range panes {
		identity := processIdentity{pid: pane.pid, sessionID: pane.pid, started: "anchored"}
		anchors = append(anchors, sessionAnchor{
			pane:      pane,
			server:    &fakeProcessHandle{identity: processIdentity{pid: pane.serverPID, sessionID: pane.serverPID, started: "server"}, exited: r.serverExited, exitErr: r.serverExitErr},
			sessionID: pane.pid,
			members:   processSet{identity: &fakeProcessHandle{identity: identity}},
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
	for _, anchor := range anchors {
		if anchor.server != nil {
			_ = anchor.server.Close()
		}
		for _, handle := range anchor.members {
			_ = handle.Close()
		}
	}
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
	if got, want := listPaneRefsArgs("sess-1"), []string{"list-panes", "-s", "-t", "=sess-1", "-F", "#{pid}\t#{start_time}\t#{pane_id}\t#{window_id}\t#{pane_pid}\t#{pane_dead}"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("listPaneRefsArgs = %#v, want %#v", got, want)
	}
	if got, want := listAllPaneRefsArgs(), []string{"list-panes", "-a", "-F", "#{pid}\t#{start_time}\t#{pane_id}\t#{window_id}"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("listAllPaneRefsArgs = %#v, want %#v", got, want)
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
	if got, want := fr.calls[0].args, listPaneRefsArgs("sess-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("list pane args = %#v, want %#v", got, want)
	}
	// killSessionArgs uses exact-match target =<id>.
	if got, want := fr.calls[1].args, killSessionArgs("sess-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("destroy args = %#v, want %#v", got, want)
	}
}

func TestDestroyReapsAllDiscoveredPaneSessions(t *testing.T) {
	r, fr := newTestRuntime(0)
	panes := []byte("100 1000 %1 @1 4242 0\n100 1000 %2 @2 4243 0\n100 1000 %3 @3 1 0\n100 1000 %4 @4 0 0\n100 1000 %1 @1 4242 0\nnoise\n")
	fr.outputs = [][]byte{panes, panes, nil, nil}
	reaper := &recordingReaper{serverExited: true}
	r.sessionReaper = reaper

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(reaper.calls) != 1 {
		t.Fatalf("reaper calls = %d, want 1", len(reaper.calls))
	}
	if got, want := reaper.anchoredPanes, []paneRef{{serverID: "100/1000", serverPID: 100, paneID: "%1", windowID: "@1", pid: 4242}, {serverID: "100/1000", serverPID: 100, paneID: "%2", windowID: "@2", pid: 4243}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("anchored panes = %#v, want %#v", got, want)
	}
	if got, want := anchorSessionIDs(reaper.calls[0].anchors), []int{4242, 4243}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reaped session ids = %#v, want %#v", got, want)
	}
	if reaper.calls[0].grace != r.reapGrace {
		t.Fatalf("reap grace = %v, want %v", reaper.calls[0].grace, r.reapGrace)
	}
}

func TestDestroyExcludesWindowLinkedAfterDiscovery(t *testing.T) {
	r, fr := newTestRuntime(0)
	panes := []byte("100 1000 %1 @1 4242 0\n100 1000 %2 @2 4243 0\n")
	fr.outputs = [][]byte{panes, panes, nil, []byte("100 1000 %2 @2\n")}
	reaper := &recordingReaper{}
	r.sessionReaper = reaper

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if got, want := anchorSessionIDs(reaper.calls[0].anchors), []int{4242}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reaped session ids = %#v, want only destroyed window %#v", got, want)
	}
}

func TestDestroyExcludesPaneMovedToAnotherWindowAfterDiscovery(t *testing.T) {
	r, fr := newTestRuntime(0)
	panes := []byte("100 1000 %1 @1 4242 0\n")
	// break-pane/join-pane/move-pane preserve the pane ID while changing its
	// window. The post-kill probe must treat the stable pane as a survivor.
	fr.outputs = [][]byte{panes, panes, nil, []byte("100 1000 %1 @9\n")}
	reaper := &recordingReaper{}
	r.sessionReaper = reaper

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(reaper.calls) != 0 {
		t.Fatalf("reaper calls = %#v, want none for pane moved to surviving window", reaper.calls)
	}
}

func TestDestroyRevalidatesPaneMovedWithinTargetSession(t *testing.T) {
	r, fr := newTestRuntime(0)
	before := []byte("100 1000 %1 @1 4242 0\n")
	afterMove := []byte("100 1000 %1 @9 4242 0\n")
	fr.outputs = [][]byte{before, afterMove, nil, []byte("100 1000 %9 @10\n")}
	reaper := &recordingReaper{}
	r.sessionReaper = reaper

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(reaper.calls) != 1 || len(reaper.calls[0].anchors) != 1 {
		t.Fatalf("reaper calls = %#v, want moved pane anchor", reaper.calls)
	}
	if got := reaper.calls[0].anchors[0].pane.windowID; got != "@9" {
		t.Fatalf("revalidated window ID = %q, want @9", got)
	}
}

func TestDestroyDoesNotConfuseReusedObjectIDsFromNewServerGeneration(t *testing.T) {
	r, fr := newTestRuntime(0)
	panes := []byte("100 1000 %1 @1 4242 0\n")
	// The original exact server and its linked/moved pane remain live, but a
	// replacement server owns the socket and restarts pane/window IDs at %1/@1.
	// Replacement-only output cannot prove the old pane disappeared.
	fr.outputs = [][]byte{panes, panes, nil, []byte("200 2000 %1 @1\n")}
	reaper := &recordingReaper{}
	r.sessionReaper = reaper

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(reaper.calls) != 0 {
		t.Fatalf("reaper calls = %#v, want fail-closed replacement-only output", reaper.calls)
	}
}

func TestDestroyFailsClosedOnEmptySuccessfulProbeWhileOriginalServerLives(t *testing.T) {
	r, fr := newTestRuntime(0)
	panes := []byte("100 1000 %1 @1 4242 0\n")
	fr.outputs = [][]byte{panes, panes, nil, nil}
	reaper := &recordingReaper{}
	r.sessionReaper = reaper

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(reaper.calls) != 0 {
		t.Fatalf("reaper calls = %#v, want fail-closed unattributed empty output", reaper.calls)
	}
}

func TestDestroyUsesExactServerHandleWhenGenerationTextCollides(t *testing.T) {
	r, fr := newTestRuntime(0)
	panes := []byte("100 1000 %1 @1 4242 0\n")
	// The replacement server reuses the numeric PID within the same second as
	// well as tmux's restarted object IDs. Only the retained old server handle
	// distinguishes this output from a surviving object in the old server.
	fr.outputs = [][]byte{panes, panes, nil, []byte("100 1000 %1 @1\n")}
	reaper := &recordingReaper{serverExited: true}
	r.sessionReaper = reaper

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if got, want := anchorSessionIDs(reaper.calls[0].anchors), []int{4242}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reaped session ids = %#v, want exact old-server anchor %#v", got, want)
	}
}

func TestDestroyReapsDuplicateWindowLinksWithinSameSession(t *testing.T) {
	r, fr := newTestRuntime(0)
	panes := []byte("100 1000 %1 @1 4242 0\n100 1000 %1 @1 4242 0\n")
	fr.outputs = [][]byte{panes, panes, nil, nil}
	reaper := &recordingReaper{serverExited: true}
	r.sessionReaper = reaper

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if got, want := anchorSessionIDs(reaper.calls[0].anchors), []int{4242}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reaped session ids = %#v, want duplicate link owned by destroyed session %#v", got, want)
	}
}

func TestDestroyRejectsPanePIDReuseBeforeAnchorCompletes(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{
		[]byte("100 1000 %1 @1 4242 0\n"),
		// The pane owner exited while Anchor ran. Even if 4242 has already been
		// reused elsewhere, tmux's dead state invalidates the numeric PID.
		[]byte("100 1000 %1 @1 4242 1\n"),
		nil,
	}
	reaper := &recordingReaper{}
	r.sessionReaper = reaper

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(reaper.calls) != 0 {
		t.Fatalf("reaper calls = %#v, want none for stale pane PID anchor", reaper.calls)
	}
}

func TestReapAfterSurvivorCheckReapsForEveryMissingServerSpellingWhenExactServerExited(t *testing.T) {
	for name, output := range map[string]string{
		"no server running":       "no server running on /tmp/tmux-1000/default",
		"socket missing":          "error connecting to /tmp/tmux-1000/default (No such file or directory)",
		"socket without listener": "error connecting to /tmp/tmux-1000/default (Connection refused)",
	} {
		t.Run(name, func(t *testing.T) {
			r, fr := newTestRuntime(0)
			fr.outputs = [][]byte{[]byte(output)}
			fr.err = &exec.ExitError{}
			reaper := &recordingReaper{}
			r.sessionReaper = reaper
			pane := paneRef{serverID: "100/1000", serverPID: 100, paneID: "%1", windowID: "@1", pid: 4242}
			identity := processIdentity{pid: 4242, sessionID: 4242, started: "anchored"}
			server := &fakeProcessHandle{identity: processIdentity{pid: 100, sessionID: 100, started: "server"}, exited: true}

			r.reapAfterSurvivorCheck(context.Background(), []sessionAnchor{{
				pane: pane, server: server, sessionID: 4242, members: processSet{identity: &fakeProcessHandle{identity: identity}},
			}})
			if len(reaper.calls) != 1 {
				t.Fatalf("reaper calls = %#v, want one after definitive missing-server output", reaper.calls)
			}
		})
	}
}

func TestReapAfterSurvivorCheckFailsClosedForMissingSocketWithoutExactServerExit(t *testing.T) {
	spellings := map[string]string{
		"no server running":       "no server running on /tmp/tmux-1000/default",
		"socket missing":          "error connecting to /tmp/tmux-1000/default (No such file or directory)",
		"socket without listener": "error connecting to /tmp/tmux-1000/default (Connection refused)",
	}
	states := map[string]int{"server live": -1, "post-missing probe ambiguous": 2}
	for spelling, output := range spellings {
		for state, errorOnCall := range states {
			t.Run(spelling+"/"+state, func(t *testing.T) {
				r, fr := newTestRuntime(0)
				fr.outputs = [][]byte{[]byte(output)}
				fr.err = &exec.ExitError{}
				reaper := &recordingReaper{}
				r.sessionReaper = reaper
				identity := processIdentity{pid: 4242, sessionID: 4242, started: "anchored"}
				server := &fakeProcessHandle{
					identity:      processIdentity{pid: 100, sessionID: 100, started: "server"},
					exitErr:       errors.New("pidfd probe unavailable"),
					exitErrOnCall: errorOnCall,
				}

				r.reapAfterSurvivorCheck(context.Background(), []sessionAnchor{{
					pane:   paneRef{serverID: "100/1000", serverPID: 100, paneID: "%1", windowID: "@1", pid: 4242},
					server: server, sessionID: 4242, members: processSet{identity: &fakeProcessHandle{identity: identity}},
				}})
				if len(reaper.calls) != 0 {
					t.Fatalf("reaper calls = %#v, want fail-closed without exact server exit", reaper.calls)
				}
			})
		}
	}
}

func TestDestroyMissingSocketDoesNotReapLinkedPaneWhileExactServerLives(t *testing.T) {
	r, _ := newTestRuntime(0)
	panes := []byte("100 1000 %1 @1 4242 0\n")
	listCalls := 0
	r.runner = runnerFunc(func(_ context.Context, _ []string, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "list-panes":
			listCalls++
			if listCalls <= 2 {
				return panes, nil
			}
			// The target link is gone and the socket path is unavailable, but
			// the retained exact server (and linked pane elsewhere) is live.
			return []byte("error connecting to /tmp/tmux-1000/default (No such file or directory)"), &exec.ExitError{}
		case "kill-session":
			return nil, nil
		default:
			t.Fatalf("unexpected tmux command %q", args[0])
			return nil, nil
		}
	})
	reaper := &recordingReaper{}
	r.sessionReaper = reaper

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(reaper.calls) != 0 {
		t.Fatalf("reaper calls = %#v, want no signals while exact server remains live", reaper.calls)
	}
}

func TestDestroyFailsClosedOnAmbiguousSurvivorVerificationError(t *testing.T) {
	r, _ := newTestRuntime(0)
	panes := []byte("100 1000 %1 @1 4242 0\n")
	call := 0
	r.runner = runnerFunc(func(_ context.Context, _ []string, _ string, args ...string) ([]byte, error) {
		call++
		if call <= 2 {
			return panes, nil
		}
		if args[0] == "kill-session" {
			return nil, nil
		}
		return []byte("error connecting to /tmp/tmux-1000/default (Permission denied)"), &exec.ExitError{}
	})
	reaper := &recordingReaper{}
	r.sessionReaper = reaper
	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(reaper.calls) != 0 {
		t.Fatalf("reaper calls = %#v, want fail-closed survivor check", reaper.calls)
	}
}

func TestDestroyDoesNotReapWhenKillSessionFails(t *testing.T) {
	r, _ := newTestRuntime(0)
	fr := &fakeRunnerSelectiveErr{
		exitErrOn: "kill-session",
		errOutput: []byte("permission denied"),
		outputs: map[string][]byte{
			"list-panes": []byte("100 1000 %1 @1 4242 0\n"),
		},
	}
	r.runner = fr
	reaper := &recordingReaper{}
	r.sessionReaper = reaper

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err == nil {
		t.Fatal("Destroy: got nil, want kill-session error")
	}
	if len(reaper.calls) != 0 {
		t.Fatalf("reaper calls = %#v, want none after unexpected kill-session failure", reaper.calls)
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

func TestDestroyGivesPaneRevalidationAnIndependentBudget(t *testing.T) {
	r, _ := newTestRuntime(0)
	r.timeout = 10 * time.Millisecond
	panes := []byte("100 1000 %1 @1 4242 0\n")
	listCalls := 0
	r.runner = runnerFunc(func(commandCtx context.Context, _ []string, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "list-panes":
			listCalls++
			if err := commandCtx.Err(); err != nil {
				t.Fatalf("list-panes call %d started with expired context: %v", listCalls, err)
			}
			if listCalls <= 2 {
				return panes, nil
			}
			return []byte("100 1000 %9 @9\n"), nil
		case "kill-session":
			if err := commandCtx.Err(); err != nil {
				t.Fatalf("kill-session received exhausted context: %v", err)
			}
			return nil, nil
		default:
			t.Fatalf("unexpected tmux command %q", args[0])
			return nil, nil
		}
	})
	reaper := &recordingReaper{anchorHook: func(anchorCtx context.Context) {
		<-anchorCtx.Done()
	}}
	r.sessionReaper = reaper

	if err := r.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if listCalls != 3 {
		t.Fatalf("list-panes calls = %d, want discovery, revalidation, and survivor probe", listCalls)
	}
	if len(reaper.calls) != 1 {
		t.Fatalf("reaper calls = %#v, want anchor preserved after slow Anchor", reaper.calls)
	}
}

func TestDestroyCallerCancellationDuringDiscoveryDoesNotSkipKillSession(t *testing.T) {
	r, _ := newTestRuntime(0)
	r.timeout = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	killed := false
	r.runner = runnerFunc(func(commandCtx context.Context, _ []string, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "list-panes":
			cancel()
			<-commandCtx.Done()
			if !errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
				t.Fatalf("discovery context error = %v, want independent bound", commandCtx.Err())
			}
			return nil, commandCtx.Err()
		case "kill-session":
			if err := commandCtx.Err(); err != nil {
				t.Fatalf("kill-session received mid-discovery cancellation: %v", err)
			}
			killed = true
			return nil, nil
		default:
			t.Fatalf("unexpected tmux command %q", args[0])
			return nil, nil
		}
	})

	if err := r.Destroy(ctx, ports.RuntimeHandle{ID: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !killed {
		t.Fatal("kill-session was skipped after caller cancellation")
	}
}

type recordingSignaler struct {
	calls        []signalCall
	handles      []*fakeProcessHandle
	errs         map[int]error
	beforeSignal func(processIdentity, os.Signal)
}

type signalCall struct {
	pid    int
	signal os.Signal
}

type fakeProcessHandle struct {
	identity      processIdentity
	table         *fakeProcessTable
	closed        bool
	exited        bool
	exitErr       error
	exitCalls     int
	exitErrOnCall int
}

func (h *fakeProcessHandle) Alive(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if h == nil || h.closed {
		return errors.New("process handle closed")
	}
	if h.exited {
		return errors.New("retained process exited")
	}
	if h.table != nil {
		current, ok := h.table.current[h.identity.pid]
		if !ok || current != h.identity {
			return errors.New("retained process exited")
		}
	}
	return nil
}

func (h *fakeProcessHandle) Exited(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if h == nil || h.closed {
		return false, errors.New("process handle closed")
	}
	h.exitCalls++
	if h.exitErr != nil && (h.exitErrOnCall == 0 || h.exitErrOnCall == h.exitCalls) {
		return false, h.exitErr
	}
	if h.exited {
		return true, nil
	}
	if h.table != nil {
		current, ok := h.table.current[h.identity.pid]
		return !ok || current != h.identity, nil
	}
	return false, nil
}

func (h *fakeProcessHandle) Signal(ctx context.Context, signal os.Signal) error {
	if h.table != nil && h.table.signaler != nil && h.table.signaler.beforeSignal != nil {
		h.table.signaler.beforeSignal(h.identity, signal)
	}
	if err := h.Alive(ctx); err != nil {
		return err
	}
	if h.table != nil && h.table.signaler != nil {
		h.table.signaler.calls = append(h.table.signaler.calls, signalCall{pid: h.identity.pid, signal: signal})
		h.table.signaler.handles = append(h.table.signaler.handles, h)
		return h.table.signaler.errs[h.identity.pid]
	}
	return nil
}

func (h *fakeProcessHandle) Close() error {
	if h != nil {
		h.closed = true
	}
	return nil
}

type processSnapshotStep struct {
	processes []processIdentity
	current   map[int]processIdentity
	err       error
}

type fakeProcessTable struct {
	current       map[int]processIdentity
	snapshots     []processSnapshotStep
	openCalls     []int
	identityDelay time.Duration
	snapshotDelay time.Duration
	snapshotCalls int
	signaler      *recordingSignaler
	handles       []*fakeProcessHandle
}

func (f *fakeProcessTable) Open(ctx context.Context, pid int) (processObservation, error) {
	if f.identityDelay > 0 {
		select {
		case <-time.After(f.identityDelay):
		case <-ctx.Done():
			return processObservation{}, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return processObservation{}, err
	}
	f.openCalls = append(f.openCalls, pid)
	identity, ok := f.current[pid]
	if !ok {
		return processObservation{}, errors.New("process missing")
	}
	handle := &fakeProcessHandle{identity: identity, table: f}
	f.handles = append(f.handles, handle)
	return processObservation{identity: identity, handle: handle}, nil
}

func (f *fakeProcessTable) Snapshot(ctx context.Context, _ map[int]struct{}) ([]processObservation, error) {
	f.snapshotCalls++
	if f.snapshotDelay > 0 {
		select {
		case <-time.After(f.snapshotDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
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
	observations := make([]processObservation, 0, len(step.processes))
	for _, identity := range step.processes {
		handle := &fakeProcessHandle{identity: identity, table: f}
		f.handles = append(f.handles, handle)
		observations = append(observations, processObservation{identity: identity, handle: handle})
	}
	return observations, step.err
}

func identity(pid, sid int, started string) processIdentity {
	return processIdentity{pid: pid, sessionID: sid, started: started}
}

func anchor(table *fakeProcessTable, sid int, members ...processIdentity) sessionAnchor {
	set := make(processSet, len(members))
	for _, member := range members {
		handle := &fakeProcessHandle{identity: member, table: table}
		table.handles = append(table.handles, handle)
		set[member] = handle
	}
	return sessionAnchor{sessionID: sid, members: set}
}

func newTestProcessReaper(table *fakeProcessTable, signaler *recordingSignaler) processSessionReaper {
	table.signaler = signaler
	return processSessionReaper{
		table:   table,
		timeout: time.Second,
		wait: func(ctx context.Context, _ time.Duration) bool {
			return ctx.Err() == nil
		},
	}
}

func assertAllHandlesClosed(t *testing.T, table *fakeProcessTable) {
	t.Helper()
	for i, handle := range table.handles {
		if !handle.closed {
			t.Fatalf("process handle %d for %#v was not closed", i, handle.identity)
		}
	}
}

func TestProcessSessionReaperAnchorsOwnedSessionBeforeTeardown(t *testing.T) {
	server := identity(100, 100, "server-start")
	leader := identity(4242, 4242, "leader-start")
	child := identity(5000, 4242, "child-start")
	notLeader := identity(6000, 4242, "not-a-session-leader")
	table := &fakeProcessTable{
		current: map[int]processIdentity{100: server, 4242: leader, 6000: notLeader},
		snapshots: []processSnapshotStep{{
			processes: []processIdentity{leader, child},
		}},
	}
	reaper := newTestProcessReaper(table, &recordingSignaler{})

	anchors := reaper.Anchor(context.Background(), []paneRef{
		{serverID: "100/1000", serverPID: 100, paneID: "%bad0", windowID: "@0", pid: 0},
		{serverID: "100/1000", serverPID: 100, paneID: "%bad1", windowID: "@1", pid: 1},
		{serverID: "100/1000", serverPID: 100, paneID: "%1", windowID: "@2", pid: 4242},
		{serverID: "100/1000", serverPID: 100, paneID: "%1", windowID: "@2", pid: 4242},
		{serverID: "100/1000", serverPID: 100, paneID: "%2", windowID: "@3", pid: 6000},
	})
	if got, want := anchorSessionIDs(anchors), []int{4242}; !reflect.DeepEqual(got, want) {
		t.Fatalf("anchor session ids = %#v, want %#v", got, want)
	}
	if _, ok := anchors[0].members[child]; !ok {
		t.Fatalf("anchor members = %#v, want pre-teardown child %#v", anchors[0].members, child)
	}
	if got, want := table.openCalls, []int{100, 4242, 100, 6000}; !reflect.DeepEqual(got, want) {
		t.Fatalf("open calls = %#v, want one retained handle per pane leader %#v", got, want)
	}
	closeAnchors(anchors)
	assertAllHandlesClosed(t, table)
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
			// Snapshot saw the old identity, but the immediate pre-KILL check
			// sees a reused PID in another session.
			{processes: []processIdentity{process}, current: map[int]processIdentity{5000: reused}},
		},
	}
	signaler := &recordingSignaler{}
	reaper := newTestProcessReaper(table, signaler)
	reaper.Reap(context.Background(), []sessionAnchor{anchor(table, 4242, process)}, 4*time.Second)

	if got, want := signaler.calls, []signalCall{{pid: 5000, signal: syscall.SIGTERM}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("signals = %#v, want no KILL after immediate identity mismatch %#v", got, want)
	}
	assertAllHandlesClosed(t, table)
}

func TestProcessSessionReaperRejectsStartOrSessionReuseBeforeTerm(t *testing.T) {
	original := identity(5000, 4242, "original")
	for name, replacement := range map[string]processIdentity{
		"start identity": identity(5000, 4242, "reused-start"),
		"session id":     identity(5000, 9999, "reused-session"),
	} {
		t.Run(name, func(t *testing.T) {
			table := &fakeProcessTable{
				current: map[int]processIdentity{5000: replacement},
				snapshots: []processSnapshotStep{{
					processes: []processIdentity{original},
				}},
			}
			signaler := &recordingSignaler{}
			reaper := newTestProcessReaper(table, signaler)
			reaper.Reap(context.Background(), []sessionAnchor{anchor(table, 4242, original)}, time.Second)
			if len(signaler.calls) != 0 {
				t.Fatalf("signals = %#v, want none after pre-TERM identity change", signaler.calls)
			}
		})
	}
}

func TestProcessSessionReaperRetainsExactHandleThroughEscalation(t *testing.T) {
	process := identity(5000, 4242, "original")
	table := &fakeProcessTable{
		current: map[int]processIdentity{5000: process},
		snapshots: []processSnapshotStep{
			{processes: []processIdentity{process}},
			{processes: []processIdentity{process}},
			{processes: []processIdentity{process}},
			{processes: []processIdentity{process}},
			{processes: []processIdentity{process}},
			{processes: []processIdentity{process}},
		},
	}
	signaler := &recordingSignaler{}
	reaper := newTestProcessReaper(table, signaler)
	anchored := anchor(table, 4242, process)
	retained := anchored.members[process]
	reaper.Reap(context.Background(), []sessionAnchor{anchored}, 0)
	if len(signaler.handles) != 2 || signaler.handles[0] != retained || signaler.handles[1] != retained {
		t.Fatalf("delivery handles = %#v, want the exact anchored handle for TERM and KILL", signaler.handles)
	}
	assertAllHandlesClosed(t, table)
}

func TestProcessSessionReaperRetainedHandleFailsClosedOnReuseAtDelivery(t *testing.T) {
	original := identity(5000, 4242, "original")
	reused := identity(5000, 9999, "reused")
	table := &fakeProcessTable{
		current:   map[int]processIdentity{5000: original},
		snapshots: []processSnapshotStep{{processes: []processIdentity{original}}},
	}
	signaler := &recordingSignaler{}
	signaler.beforeSignal = func(_ processIdentity, signal os.Signal) {
		if signal == syscall.SIGTERM {
			// Reuse happens in the old validate-then-numeric-kill window. The
			// retained handle must fail rather than deliver to this generation.
			table.current = map[int]processIdentity{5000: reused}
		}
	}
	reaper := newTestProcessReaper(table, signaler)
	reaper.Reap(context.Background(), []sessionAnchor{anchor(table, 4242, original)}, time.Second)
	if len(signaler.calls) != 0 {
		t.Fatalf("signals = %#v, want retained handle to reject reused delivery target", signaler.calls)
	}
	assertAllHandlesClosed(t, table)
}

func TestProcessSessionReaperRejectsStartOrSessionReuseBeforeKill(t *testing.T) {
	original := identity(5000, 4242, "original")
	for name, replacement := range map[string]processIdentity{
		"start identity": identity(5000, 4242, "reused-start"),
		"session id":     identity(5000, 9999, "reused-session"),
	} {
		t.Run(name, func(t *testing.T) {
			table := &fakeProcessTable{
				current: map[int]processIdentity{5000: original},
				snapshots: []processSnapshotStep{
					{processes: []processIdentity{original}},
					{processes: []processIdentity{original}},
					{processes: []processIdentity{original}},
					{processes: []processIdentity{original}, current: map[int]processIdentity{5000: replacement}},
				},
			}
			signaler := &recordingSignaler{}
			reaper := newTestProcessReaper(table, signaler)
			reaper.Reap(context.Background(), []sessionAnchor{anchor(table, 4242, original)}, 4*time.Second)
			want := []signalCall{{pid: 5000, signal: syscall.SIGTERM}}
			if !reflect.DeepEqual(signaler.calls, want) {
				t.Fatalf("signals = %#v, want TERM only after pre-KILL identity change %#v", signaler.calls, want)
			}
		})
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
		if duration != 800*time.Millisecond {
			t.Fatalf("grace slice = %v, want 800ms", duration)
		}
		return true
	}

	reaper.Reap(context.Background(), []sessionAnchor{anchor(table, 4242, original)}, 4*time.Second)
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

func TestProcessSessionReaperEscalatesChildAppearingAfterFourthSnapshot(t *testing.T) {
	original := identity(5000, 4242, "original")
	late := identity(5001, 4242, "final-slice")
	table := &fakeProcessTable{
		current: map[int]processIdentity{5000: original},
		snapshots: []processSnapshotStep{
			{processes: []processIdentity{original}},
			{processes: []processIdentity{original}},
			{processes: []processIdentity{original}},
			{processes: []processIdentity{original}},
			// This fifth observation runs after the fourth wait.
			{processes: []processIdentity{original, late}, current: map[int]processIdentity{5000: original, 5001: late}},
			{processes: []processIdentity{original, late}},
		},
	}
	signaler := &recordingSignaler{}
	reaper := newTestProcessReaper(table, signaler)
	reaper.Reap(context.Background(), []sessionAnchor{anchor(table, 4242, original)}, 4*time.Second)
	want := []signalCall{
		{pid: 5000, signal: syscall.SIGTERM},
		{pid: 5001, signal: syscall.SIGTERM},
		{pid: 5000, signal: syscall.SIGKILL},
		{pid: 5001, signal: syscall.SIGKILL},
	}
	if !reflect.DeepEqual(signaler.calls, want) {
		t.Fatalf("signals = %#v, want post-fourth-wait child grace and escalation %#v", signaler.calls, want)
	}
}

func TestProcessSessionReaperObservesChildCreatedDuringFinalGraceWait(t *testing.T) {
	original := identity(5000, 4242, "original")
	late := identity(5001, 4242, "final-wait")
	unrelated := identity(6000, 9999, "unrelated")
	table := &fakeProcessTable{
		current: map[int]processIdentity{5000: original},
		snapshots: []processSnapshotStep{
			{processes: []processIdentity{original}},
			{processes: []processIdentity{original}},
			{processes: []processIdentity{original}},
			{processes: []processIdentity{original}},
			{processes: []processIdentity{original}},
			{processes: []processIdentity{original, late, unrelated}},
		},
	}
	signaler := &recordingSignaler{}
	reaper := newTestProcessReaper(table, signaler)
	waits := 0
	reaper.wait = func(ctx context.Context, _ time.Duration) bool {
		waits++
		if waits == reapGraceSlices {
			table.current = map[int]processIdentity{5000: original, 5001: late, 6000: unrelated}
		}
		return ctx.Err() == nil
	}
	reaper.Reap(context.Background(), []sessionAnchor{anchor(table, 4242, original)}, 5*time.Second)

	want := []signalCall{
		{pid: original.pid, signal: syscall.SIGTERM},
		{pid: late.pid, signal: syscall.SIGTERM},
		{pid: original.pid, signal: syscall.SIGKILL},
		{pid: late.pid, signal: syscall.SIGKILL},
	}
	if !reflect.DeepEqual(signaler.calls, want) {
		t.Fatalf("signals = %#v, want final-wait child only %#v", signaler.calls, want)
	}
}

func TestProcessSessionReaperBudgetsEverySlowProbe(t *testing.T) {
	process := identity(5000, 4242, "original")
	table := &fakeProcessTable{
		current:       map[int]processIdentity{5000: process},
		identityDelay: 2 * time.Millisecond,
		snapshotDelay: 2 * time.Millisecond,
		snapshots: []processSnapshotStep{
			{processes: []processIdentity{process}},
			{processes: []processIdentity{process}},
			{processes: []processIdentity{process}},
			{processes: []processIdentity{process}},
			{processes: []processIdentity{process}},
			{processes: []processIdentity{process}},
		},
	}
	signaler := &recordingSignaler{}
	reaper := newTestProcessReaper(table, signaler)
	reaper.timeout = 5 * time.Millisecond
	reaper.Reap(context.Background(), []sessionAnchor{anchor(table, 4242, process)}, 0)
	if table.snapshotCalls != reapObservations {
		t.Fatalf("snapshot calls = %d, want %d independently budgeted probes", table.snapshotCalls, reapObservations)
	}
	want := []signalCall{{pid: 5000, signal: syscall.SIGTERM}, {pid: 5000, signal: syscall.SIGKILL}}
	if !reflect.DeepEqual(signaler.calls, want) {
		t.Fatalf("signals = %#v, want cleanup to outlive aggregate probe timeout %#v", signaler.calls, want)
	}
	assertAllHandlesClosed(t, table)
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
	reaper.Reap(context.Background(), []sessionAnchor{anchor(table, 4242, original)}, 5*time.Second)
	if len(signaler.calls) != 0 {
		t.Fatalf("signals = %#v, want none without an anchored continuity witness", signaler.calls)
	}
}

func TestProcessSessionReaperFallsBackToAnotherExactLiveWitness(t *testing.T) {
	leader := identity(4242, 4242, "leader")
	child := identity(5000, 4242, "child")
	late := identity(5001, 4242, "late")
	all := []processIdentity{leader, child, late}
	table := &fakeProcessTable{
		current: map[int]processIdentity{4242: leader, 5000: child, 5001: late},
		snapshots: []processSnapshotStep{
			{processes: all},
			{processes: all},
			{processes: all},
			{processes: all},
			{processes: all},
			{processes: all},
		},
	}
	signaler := &recordingSignaler{}
	signaler.beforeSignal = func(target processIdentity, signal os.Signal) {
		if target == leader && signal == syscall.SIGTERM {
			delete(table.current, leader.pid)
		}
	}
	reaper := newTestProcessReaper(table, signaler)
	reaper.Reap(context.Background(), []sessionAnchor{anchor(table, 4242, leader, child)}, 0)

	want := []signalCall{
		{pid: child.pid, signal: syscall.SIGTERM},
		{pid: late.pid, signal: syscall.SIGTERM},
		{pid: child.pid, signal: syscall.SIGKILL},
		{pid: late.pid, signal: syscall.SIGKILL},
	}
	if !reflect.DeepEqual(signaler.calls, want) {
		t.Fatalf("signals = %#v, want fallback witness to retain session %#v", signaler.calls, want)
	}
	assertAllHandlesClosed(t, table)
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
			{processes: []processIdentity{process}},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	signaler := &recordingSignaler{}
	reaper := newTestProcessReaper(table, signaler)
	reaper.Reap(ctx, []sessionAnchor{anchor(table, 4242, process)}, 4*time.Second)
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
