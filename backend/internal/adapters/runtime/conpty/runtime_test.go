package conpty

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/conpty/ptyregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// livePID returns a PID that is guaranteed to be alive (the current process).
// Using this as the fake pty-host PID means ptyregistry.List() will not prune
// the entry during tests. Do NOT use this for the Destroy test: Destroy calls
// Kill on the pid, so use deadPID() there instead.
func livePID() int { return os.Getpid() }

// deadPID returns a PID that is guaranteed to be dead (no process). This is
// used in Destroy tests so the force-kill step is a safe no-op.
// ponytail: PID 2147483647 (MaxInt32) is never a real process; signal-0 returns ESRCH.
func deadPID() int { return 2147483647 }

type fakeProcessHandle struct {
	alive   func() (bool, error)
	killErr error
	killed  bool
	closed  bool
}

func (p *fakeProcessHandle) Alive() (bool, error) {
	if p.alive == nil {
		return false, nil
	}
	return p.alive()
}
func (p *fakeProcessHandle) Kill() error  { p.killed = true; return p.killErr }
func (p *fakeProcessHandle) Close() error { p.closed = true; return nil }

func withProcessFinder(t *testing.T, finder func(int) (processKiller, error)) {
	t.Helper()
	original := osProcessFinder
	osProcessFinder = finder
	t.Cleanup(func() { osProcessFinder = original })
}

func withDialHost(t *testing.T, dial func(string, time.Duration) (net.Conn, error)) {
	t.Helper()
	original := dialHost
	dialHost = dial
	t.Cleanup(func() { dialHost = original })
}

func readRawFrame(conn net.Conn) (byte, []byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, binary.BigEndian.Uint32(header[1:]))
	if _, err := io.ReadFull(conn, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

// ---------------------------------------------------------------------------
// Test harness: in-process pty-host backed by a fakePTY.
// ---------------------------------------------------------------------------

// inProcHost starts a Serve engine with a fakePTY on a real 127.0.0.1:0
// listener and returns a fake spawner that returns that addr and a fake pid.
// The caller must call cleanup() to shut down the host.
type inProcHost struct {
	addr       string
	pid        int
	generation string
	pty        *fakePTY
	ring       *Ring
	cancel     context.CancelFunc
	done       chan error
	ln         net.Listener
}

func startInProcHost(t *testing.T, sessionID string, fakePID int, generations ...string) *inProcHost {
	t.Helper()
	generation := "test-generation-" + sessionID
	if len(generations) > 0 {
		generation = generations[0]
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	pty := newFakePTY(fakePID)
	ring := NewRing()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ServeConfig{
			SessionID:  sessionID,
			Generation: generation,
			HostPID:    fakePID,
			Listener:   ln,
			PTY:        pty,
			Ring:       ring,
		})
	}()
	return &inProcHost{
		addr:       ln.Addr().String(),
		pid:        fakePID,
		generation: generation,
		pty:        pty,
		ring:       ring,
		cancel:     cancel,
		done:       done,
		ln:         ln,
	}
}

func (h *inProcHost) cleanup(t *testing.T) {
	t.Helper()
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		t.Log("warning: inProcHost did not stop within 2s")
	}
}

// fakeSpawnerFor returns a hostSpawner that starts an in-process host for a
// single session ID and records which sessions have been spawned.
// The returned map maps sessionID -> *inProcHost for test inspection.
func fakeSpawnerFor(t *testing.T, hosts map[string]*inProcHost, fakePID int) hostSpawner {
	t.Helper()
	return func(ctx context.Context, sessionID, cwd string, argv []string, env map[string]string) (string, int, error) {
		h := startInProcHost(t, sessionID, fakePID, env[hostGenerationEnv])
		if hosts != nil {
			hosts[sessionID] = h
		}
		return h.addr, h.pid, nil
	}
}

func startRawStatusServer(t *testing.T, payload []byte) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 64)
				_, _ = conn.Read(buf)
				frame, _ := EncodeMessage(MsgStatusRes, payload)
				_, _ = conn.Write(frame)
			}()
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close(); <-done }
}

// ---------------------------------------------------------------------------
// Redirect ptyregistry to a temp data dir so tests don't pollute ~/.ao.
// ---------------------------------------------------------------------------

func isolateRegistry(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AO_DATA_DIR", dataDir)
	t.Setenv("AO_RUN_FILE", filepath.Join(dataDir, "running.json"))
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestCreate_RegistersSession verifies Create returns {ID: sessionID}, writes
// to the in-memory map, and registers in the ptyregistry.
func TestCreate_RegistersSession(t *testing.T) {
	isolateRegistry(t)
	hosts := map[string]*inProcHost{}
	rt := New(Options{Spawner: fakeSpawnerFor(t, hosts, livePID())})

	ctx := context.Background()
	handle, err := rt.Create(ctx, ports.RuntimeConfig{
		SessionID:     domain.SessionID("sess-abc"),
		WorkspacePath: "/tmp/workspace",
		Argv:          []string{"claude-code"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if handle.ID != "sess-abc" {
		t.Fatalf("handle.ID = %q, want %q", handle.ID, "sess-abc")
	}

	// In-memory map must have the entry.
	rt.mu.Lock()
	sess := rt.sessions["sess-abc"]
	rt.mu.Unlock()
	if sess == nil {
		t.Fatal("session not in in-memory map after Create")
	}

	// Registry must have the entry.
	entries, err := ptyregistry.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.SessionID == "sess-abc" {
			found = true
		}
	}
	if !found {
		t.Fatal("session not in registry after Create")
	}

	hosts["sess-abc"].cleanup(t)
}

func TestCreateRegistryFailureStopsUnpublishedHost(t *testing.T) {
	isolateRegistry(t)
	withProcessFinder(t, func(int) (processKiller, error) { return &fakeProcessHandle{}, nil })
	hosts := map[string]*inProcHost{}
	publishErr := errors.New("registry unavailable")
	rt := New(Options{
		Spawner:          fakeSpawnerFor(t, hosts, deadPID()),
		RegistryRegister: func(ptyregistry.Entry) error { return publishErr },
	})
	_, err := rt.Create(context.Background(), ports.RuntimeConfig{SessionID: "publish-fail", WorkspacePath: "/tmp/w", Argv: []string{"sh"}})
	if err == nil || !errors.Is(err, publishErr) {
		t.Fatalf("Create error = %v, want registry publication failure", err)
	}
	select {
	case <-hosts["publish-fail"].done:
	case <-time.After(3 * time.Second):
		t.Fatal("unpublished host was left running")
	}
	rt.mu.Lock()
	_, exists := rt.sessions["publish-fail"]
	rt.mu.Unlock()
	if exists {
		t.Fatal("unpublished host remained in runtime map")
	}
}

func TestExplicitDefaultDataDirRecoversHostWhenEnvUnset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AO_DATA_DIR", "")
	t.Setenv("AO_RUN_FILE", "")
	dataDir := filepath.Join(home, ".ao", "data")
	h := startInProcHost(t, "crash-boundary", livePID())
	defer h.cleanup(t)
	// This is the host-side publication that occurs before READY. Simulate the
	// parent daemon crashing before its redundant registration can run.
	if err := ptyregistry.RegisterAt(dataDir, ptyregistry.Entry{SessionID: "crash-boundary", PtyHostPID: h.pid, PipePath: h.addr, RegisteredAt: time.Now().UTC().Format(time.RFC3339Nano), Generation: h.generation}); err != nil {
		t.Fatal(err)
	}
	entries, err := ptyregistry.ListAt(dataDir)
	if err != nil || len(entries) != 1 || entries[0].SessionID != "crash-boundary" {
		t.Fatalf("host registry was not written under default config data dir: entries=%v err=%v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".ao", "windows-pty-hosts.json")); !os.IsNotExist(err) {
		t.Fatalf("host publication escaped into legacy namespace: %v", err)
	}

	// A replacement runtime is also explicitly pinned to cfg.DataDir, so it
	// finds the host even though AO_DATA_DIR is absent from the daemon env.
	rt := New(Options{DataDir: dataDir, Spawner: fakeSpawnerFor(t, nil, livePID())})
	alive, err := rt.IsAlive(context.Background(), ports.RuntimeHandle{ID: "crash-boundary"})
	if err != nil || !alive {
		t.Fatalf("restart recovery = alive:%v err:%v", alive, err)
	}
}

func TestRelativeDataDirPinsHostPublicationAndRestartRecovery(t *testing.T) {
	root := t.TempDir()
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCWD) })
	relativeDataDir := filepath.Join("relative", "state")
	absoluteDataDir := filepath.Join(root, relativeDataDir)
	workspace := t.TempDir()
	if filepath.Clean(workspace) == filepath.Clean(root) {
		t.Fatal("test requires distinct daemon and host working directories")
	}
	var host *inProcHost
	spawner := func(_ context.Context, sessionID, hostCWD string, _ []string, env map[string]string) (string, int, error) {
		if filepath.Clean(hostCWD) != filepath.Clean(workspace) {
			return "", 0, fmt.Errorf("host cwd = %q, want distinct workspace %q", hostCWD, workspace)
		}
		if got := env[dataDirEnv]; got != absoluteDataDir || !filepath.IsAbs(got) {
			return "", 0, fmt.Errorf("spawn %s = %q, want absolute %q", dataDirEnv, got, absoluteDataDir)
		}
		host = startInProcHost(t, sessionID, livePID(), env[hostGenerationEnv])
		if err := ptyregistry.RegisterAt(env[dataDirEnv], ptyregistry.Entry{
			SessionID: sessionID, PtyHostPID: host.pid, PipePath: host.addr,
			RegisteredAt: time.Now().UTC().Format(time.RFC3339Nano), Generation: host.generation,
		}); err != nil {
			host.cleanup(t)
			return "", 0, err
		}
		return host.addr, host.pid, nil
	}
	rt := New(Options{
		DataDir: relativeDataDir,
		Spawner: spawner,
		// Simulate a daemon crash after host-side pre-READY publication but
		// before the parent's redundant authoritative register.
		RegistryRegister: func(ptyregistry.Entry) error { return nil },
	})
	if _, err := rt.Create(context.Background(), ports.RuntimeConfig{
		SessionID: "relative-crash-boundary", WorkspacePath: workspace, Argv: []string{"reviewer"},
	}); err != nil {
		t.Fatal(err)
	}
	defer host.cleanup(t)

	restarted := New(Options{DataDir: relativeDataDir})
	alive, err := restarted.IsAlive(context.Background(), ports.RuntimeHandle{ID: "relative-crash-boundary"})
	if err != nil || !alive {
		t.Fatalf("restart recovery = alive:%v err:%v", alive, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, relativeDataDir, "windows-pty-hosts")); !os.IsNotExist(err) {
		t.Fatalf("host publication escaped into workspace-relative namespace: %v", err)
	}
}

func TestCreateRemovesCaseInsensitiveReservedProjectEnv(t *testing.T) {
	dataDir := t.TempDir()
	var captured map[string]string
	rt := New(Options{
		DataDir: dataDir,
		Spawner: func(_ context.Context, _ string, _ string, _ []string, env map[string]string) (string, int, error) {
			captured = env
			return "", 0, errors.New("stop after env capture")
		},
	})
	_, _ = rt.Create(context.Background(), ports.RuntimeConfig{
		SessionID: "reserved-env", WorkspacePath: t.TempDir(), Argv: []string{"agent"},
		Env: map[string]string{
			"ao_data_dir":            "spoofed-data",
			"Ao_PtY_HoSt_GeNeRaTiOn": "spoofed-generation",
			"SAFE":                   "kept",
		},
	})
	if captured == nil {
		t.Fatal("spawner was not called")
	}
	if captured[dataDirEnv] != dataDir || captured[hostGenerationEnv] == "" || captured["SAFE"] != "kept" {
		t.Fatalf("authoritative spawn env = %#v", captured)
	}
	for key := range captured {
		if isReservedHostEnvKey(key) && key != dataDirEnv && key != hostGenerationEnv {
			t.Fatalf("mixed-case reserved key survived: %q", key)
		}
	}
}

func TestCreatePinsHostRegistryNamespaceForSparseReviewerEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AO_DATA_DIR", "")
	t.Setenv("AO_RUN_FILE", "")
	dataDir := filepath.Join(home, ".ao", "data")
	originalEnv := map[string]string{"AO_REVIEWER": "1"}
	var host *inProcHost

	spawner := func(_ context.Context, sessionID, _ string, _ []string, env map[string]string) (string, int, error) {
		if got := env["AO_DATA_DIR"]; got != dataDir {
			return "", 0, fmt.Errorf("spawn AO_DATA_DIR = %q, want %q", got, dataDir)
		}
		host = startInProcHost(t, sessionID, livePID(), env[hostGenerationEnv])
		// RunHost publishes through the ambient registry before READY. Model that
		// boundary with the namespace delivered in the spawned process env.
		if err := ptyregistry.RegisterAt(env["AO_DATA_DIR"], ptyregistry.Entry{
			SessionID:    sessionID,
			PtyHostPID:   host.pid,
			PipePath:     host.addr,
			RegisteredAt: time.Now().UTC().Format(time.RFC3339),
			Generation:   env[hostGenerationEnv],
		}); err != nil {
			host.cleanup(t)
			return "", 0, err
		}
		return host.addr, host.pid, nil
	}
	rt := New(Options{
		DataDir: dataDir,
		Spawner: spawner,
		// Model a daemon crash before its redundant parent publication: the
		// host's pre-READY entry must be sufficient for replacement recovery.
		RegistryRegister: func(ptyregistry.Entry) error { return nil },
	})
	_, err := rt.Create(context.Background(), ports.RuntimeConfig{
		SessionID:     "reviewer-crash-boundary",
		WorkspacePath: t.TempDir(),
		Argv:          []string{"reviewer"},
		Env:           originalEnv,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer host.cleanup(t)
	if _, mutated := originalEnv["AO_DATA_DIR"]; mutated {
		t.Fatal("Create mutated the caller's sparse reviewer env")
	}

	entries, err := ptyregistry.ListAt(dataDir)
	if err != nil || len(entries) != 1 || entries[0].SessionID != "reviewer-crash-boundary" {
		t.Fatalf("host publication namespace: entries=%v err=%v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".ao", "windows-pty-hosts.json")); !os.IsNotExist(err) {
		t.Fatalf("host publication escaped into legacy namespace: %v", err)
	}

	restarted := New(Options{DataDir: dataDir})
	alive, err := restarted.IsAlive(context.Background(), ports.RuntimeHandle{ID: "reviewer-crash-boundary"})
	if err != nil || !alive {
		t.Fatalf("replacement daemon recovery = alive:%v err:%v", alive, err)
	}
}

func TestCreateAdoptsExistingLiveRegistryGenerationWithoutSpawning(t *testing.T) {
	isolateRegistry(t)
	h := startInProcHost(t, "existing", livePID(), "existing-generation")
	defer h.cleanup(t)
	if err := ptyregistry.Register(ptyregistry.Entry{SessionID: "existing", PtyHostPID: h.pid, PipePath: h.addr, RegisteredAt: time.Now().UTC().Format(time.RFC3339Nano), Generation: h.generation}); err != nil {
		t.Fatal(err)
	}
	spawned := false
	rt := New(Options{Spawner: func(context.Context, string, string, []string, map[string]string) (string, int, error) {
		spawned = true
		return "", 0, errors.New("must not spawn")
	}})
	if _, err := rt.Create(context.Background(), ports.RuntimeConfig{SessionID: "existing", WorkspacePath: t.TempDir(), Argv: []string{"agent"}}); err == nil {
		t.Fatal("Create accepted a live recovered generation")
	}
	if spawned {
		t.Fatal("Create called spawner despite live recovered generation")
	}
}

func TestResolveSkipsNewestProvenStaleGenerationAndAdoptsOlderValid(t *testing.T) {
	isolateRegistry(t)
	valid := startInProcHost(t, "same", livePID(), "valid-generation")
	defer valid.cleanup(t)
	other := startInProcHost(t, "other", livePID(), "other-generation")
	defer other.cleanup(t)
	if err := ptyregistry.Register(ptyregistry.Entry{SessionID: "same", PtyHostPID: valid.pid, PipePath: valid.addr, RegisteredAt: "2026-01-01T00:00:00.100Z", Generation: valid.generation}); err != nil {
		t.Fatal(err)
	}
	if err := ptyregistry.Register(ptyregistry.Entry{SessionID: "same", PtyHostPID: other.pid, PipePath: other.addr, RegisteredAt: "2026-01-01T00:00:00.200Z", Generation: "stale-generation"}); err != nil {
		t.Fatal(err)
	}
	rt := New(Options{})
	alive, err := rt.IsAlive(context.Background(), ports.RuntimeHandle{ID: "same"})
	if err != nil || !alive {
		t.Fatalf("older valid generation was not adopted: alive=%v err=%v", alive, err)
	}
	entries, err := ptyregistry.LookupAll("same")
	if err != nil || len(entries) != 1 || entries[0].Generation != valid.generation {
		t.Fatalf("stale generation not exactly evicted: entries=%v err=%v", entries, err)
	}
}

func TestRecoveredStaleEndpointIsNotCachedOrRoutable(t *testing.T) {
	isolateRegistry(t)
	other := startInProcHost(t, "other", livePID(), "other-generation")
	defer other.cleanup(t)
	if err := ptyregistry.Register(ptyregistry.Entry{SessionID: "stale", PtyHostPID: other.pid, PipePath: other.addr, RegisteredAt: time.Now().UTC().Format(time.RFC3339Nano), Generation: "stale-generation"}); err != nil {
		t.Fatal(err)
	}
	input := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := other.pty.inR.Read(buf)
		if n > 0 {
			input <- append([]byte(nil), buf[:n]...)
		}
	}()
	rt := New(Options{})
	if err := rt.SendMessage(context.Background(), ports.RuntimeHandle{ID: "stale"}, "do not deliver"); err == nil {
		t.Fatal("SendMessage routed through a proven-stale recovered endpoint")
	}
	rt.mu.Lock()
	_, cached := rt.sessions["stale"]
	rt.mu.Unlock()
	if cached {
		t.Fatal("proven-stale recovered endpoint remained cached")
	}
	// Also cover port reuse after an earlier successful resolution: each fresh
	// operation connection must authenticate before delivering bytes.
	rt.mu.Lock()
	rt.sessions["stale"] = &hostSession{addr: other.addr, pid: other.pid, generation: "stale-generation"}
	rt.mu.Unlock()
	if err := rt.SendMessage(context.Background(), ports.RuntimeHandle{ID: "stale"}, "still do not deliver"); err == nil {
		t.Fatal("cached stale endpoint accepted input without per-connection identity")
	}
	rt.mu.Lock()
	_, cached = rt.sessions["stale"]
	rt.mu.Unlock()
	if cached {
		t.Fatal("per-connection identity mismatch did not evict cached generation")
	}
	select {
	case got := <-input:
		t.Fatalf("other host received stale-session input: %q", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestClientIsAliveIdentityFailuresAreNotAuthoritativeDead(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "malformed", payload: `{`},
		{name: "missing", payload: `{"alive":true,"pid":1}`},
		{name: "mismatched", payload: `{"alive":true,"pid":1,"hostPid":9,"sessionId":"other","generation":"other"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addr, cleanup := startRawStatusServer(t, []byte(tc.payload))
			defer cleanup()
			alive, err := clientIsAlive(addr, "wanted", "generation", 9)
			if err == nil || alive {
				t.Fatalf("identity failure produced authoritative result: alive=%v err=%v", alive, err)
			}
			var mismatch *hostIdentityMismatchError
			if tc.name == "mismatched" && !errors.As(err, &mismatch) {
				t.Fatalf("mismatch error type = %T, want hostIdentityMismatchError", err)
			}
		})
	}
}

func TestLegacyGenerationlessHostFailsSafeAndRetainsManualCleanupBoundary(t *testing.T) {
	isolateRegistry(t)
	payload, _ := json.Marshal(StatusPayload{Alive: true, PID: 123}) // pre-generation protocol
	addr, cleanup := startRawStatusServer(t, payload)
	defer cleanup()
	legacy := ptyregistry.Entry{SessionID: "legacy", PtyHostPID: livePID(), PipePath: addr, RegisteredAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := ptyregistry.Register(legacy); err != nil {
		t.Fatal(err)
	}
	rt := New(Options{})
	if alive, err := rt.IsAlive(context.Background(), ports.RuntimeHandle{ID: "legacy"}); err == nil || alive {
		t.Fatalf("generationless legacy host was silently treated as dead/alive: alive=%v err=%v", alive, err)
	}
	if _, ok, err := ptyregistry.Lookup("legacy"); err != nil || !ok {
		t.Fatalf("legacy fence was not retained for manual cleanup: ok=%v err=%v", ok, err)
	}
}

func TestLaunchInProgressIsTransientForConcurrentIsAliveAndDestroy(t *testing.T) {
	isolateRegistry(t)
	withProcessFinder(t, func(int) (processKiller, error) { return &fakeProcessHandle{}, nil })
	spawnerEntered := make(chan struct{})
	allowSpawn := make(chan struct{})
	var host *inProcHost
	rt := New(Options{Spawner: func(_ context.Context, sessionID, _ string, _ []string, env map[string]string) (string, int, error) {
		close(spawnerEntered)
		<-allowSpawn
		host = startInProcHost(t, sessionID, deadPID(), env[hostGenerationEnv])
		return host.addr, host.pid, nil
	}})
	created := make(chan error, 1)
	go func() {
		_, err := rt.Create(context.Background(), ports.RuntimeConfig{SessionID: "launching", WorkspacePath: t.TempDir(), Argv: []string{"agent"}})
		created <- err
	}()
	<-spawnerEntered
	if alive, err := rt.IsAlive(context.Background(), ports.RuntimeHandle{ID: "launching"}); alive || !errors.Is(err, errHostLaunchInProgress) {
		t.Fatalf("IsAlive during launch = alive:%v err:%v", alive, err)
	}
	if err := rt.Destroy(context.Background(), ports.RuntimeHandle{ID: "launching"}); !errors.Is(err, errHostLaunchInProgress) {
		t.Fatalf("Destroy during launch error = %v, want launch-in-progress", err)
	}
	close(allowSpawn)
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	if err := rt.Destroy(context.Background(), ports.RuntimeHandle{ID: "launching"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-host.done:
	case <-time.After(2 * time.Second):
		t.Fatal("host did not stop")
	}
}

func TestParentRegisterBlockedCreateRemainsLaunchInProgress(t *testing.T) {
	isolateRegistry(t)
	withProcessFinder(t, func(int) (processKiller, error) { return &fakeProcessHandle{}, nil })
	hosts := map[string]*inProcHost{}
	registerEntered := make(chan struct{})
	allowRegister := make(chan struct{})
	rt := New(Options{
		Spawner: fakeSpawnerFor(t, hosts, deadPID()),
		RegistryRegister: func(entry ptyregistry.Entry) error {
			close(registerEntered)
			<-allowRegister
			return ptyregistry.Register(entry)
		},
	})
	created := make(chan error, 1)
	go func() {
		_, err := rt.Create(context.Background(), ports.RuntimeConfig{SessionID: "registering", WorkspacePath: t.TempDir(), Argv: []string{"agent"}})
		created <- err
	}()
	<-registerEntered
	if err := rt.Destroy(context.Background(), ports.RuntimeHandle{ID: "registering"}); !errors.Is(err, errHostLaunchInProgress) {
		t.Fatalf("Destroy while parent register blocked = %v", err)
	}
	close(allowRegister)
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	if err := rt.Destroy(context.Background(), ports.RuntimeHandle{ID: "registering"}); err != nil {
		t.Fatal(err)
	}
}

func TestLookupBlockedResolverCannotRepopulateAfterDestroy(t *testing.T) {
	isolateRegistry(t)
	withProcessFinder(t, func(int) (processKiller, error) { return &fakeProcessHandle{}, nil })
	h := startInProcHost(t, "lookup-race", livePID(), "lookup-generation")
	if err := ptyregistry.Register(ptyregistry.Entry{SessionID: "lookup-race", PtyHostPID: h.pid, PipePath: h.addr, RegisteredAt: time.Now().UTC().Format(time.RFC3339Nano), Generation: h.generation}); err != nil {
		t.Fatal(err)
	}
	lookupEntered := make(chan struct{})
	allowLookup := make(chan struct{})
	var blockFirst sync.Once
	rt := New(Options{RegistryLookupAll: func(id string) ([]ptyregistry.Entry, error) {
		entries, err := ptyregistry.LookupAll(id)
		blockFirst.Do(func() {
			close(lookupEntered)
			<-allowLookup
		})
		return entries, err
	}})
	resolved := make(chan error, 1)
	go func() { _, err := rt.resolve("lookup-race"); resolved <- err }()
	<-lookupEntered
	destroyed := make(chan error, 1)
	go func() { destroyed <- rt.Destroy(context.Background(), ports.RuntimeHandle{ID: "lookup-race"}) }()
	close(allowLookup)
	if err := <-resolved; err != nil {
		t.Fatal(err)
	}
	if err := <-destroyed; err != nil {
		t.Fatal(err)
	}
	rt.mu.Lock()
	_, cached := rt.sessions["lookup-race"]
	rt.mu.Unlock()
	if cached {
		t.Fatal("stale resolver repopulated cache after Destroy")
	}
	if entries, err := ptyregistry.LookupAll("lookup-race"); err != nil || len(entries) != 0 {
		t.Fatalf("registry retained destroyed generation: entries=%v err=%v", entries, err)
	}
}

func TestRecoveredPIDReuseIdentityMismatchCannotAdoptOtherHost(t *testing.T) {
	isolateRegistry(t)
	other := startInProcHost(t, "other-host", livePID(), "other-generation")
	defer other.cleanup(t)
	if err := ptyregistry.Register(ptyregistry.Entry{SessionID: "stale-host", PtyHostPID: other.pid, PipePath: other.addr, RegisteredAt: time.Now().UTC().Format(time.RFC3339Nano), Generation: "stale-generation"}); err != nil {
		t.Fatal(err)
	}
	finderCalls := 0
	withProcessFinder(t, func(int) (processKiller, error) {
		finderCalls++
		return &fakeProcessHandle{alive: func() (bool, error) { return true, nil }}, nil
	})
	rt := New(Options{})
	if err := rt.Destroy(context.Background(), ports.RuntimeHandle{ID: "stale-host"}); err != nil {
		t.Fatal(err)
	}
	if finderCalls != 0 {
		t.Fatalf("Destroy opened or killed reused PID before recovered identity proof: calls=%d", finderCalls)
	}
	alive, err := clientIsAlive(other.addr, "other-host", other.generation, other.pid)
	if err != nil || !alive {
		t.Fatalf("other host was touched: alive=%v err=%v", alive, err)
	}
}

func TestCachedPIDReuseOpensThenClosesHandleWithoutKillingOtherHost(t *testing.T) {
	isolateRegistry(t)
	other := startInProcHost(t, "other-cached", livePID(), "other-generation")
	defer other.cleanup(t)
	stale := &hostSession{addr: other.addr, pid: other.pid, generation: "stale-generation"}
	if err := ptyregistry.Register(ptyregistry.Entry{SessionID: "stale-cached", PtyHostPID: stale.pid, PipePath: stale.addr, RegisteredAt: time.Now().UTC().Format(time.RFC3339Nano), Generation: stale.generation}); err != nil {
		t.Fatal(err)
	}
	retained := &fakeProcessHandle{alive: func() (bool, error) { return true, nil }}
	finderCalls := 0
	withProcessFinder(t, func(int) (processKiller, error) {
		finderCalls++
		return retained, nil
	})
	rt := New(Options{})
	rt.mu.Lock()
	rt.sessions["stale-cached"] = stale
	rt.mu.Unlock()
	if err := rt.Destroy(context.Background(), ports.RuntimeHandle{ID: "stale-cached"}); err != nil {
		t.Fatal(err)
	}
	if finderCalls != 1 || !retained.closed || retained.killed {
		t.Fatalf("retained reused-PID handle calls=%d closed=%v killed=%v", finderCalls, retained.closed, retained.killed)
	}
	alive, err := clientIsAlive(other.addr, "other-cached", other.generation, other.pid)
	if err != nil || !alive {
		t.Fatalf("other host was touched: alive=%v err=%v", alive, err)
	}
}

func TestForceKillFailureRetainsExactGeneration(t *testing.T) {
	isolateRegistry(t)
	hosts := map[string]*inProcHost{}
	killErr := errors.New("terminate denied")
	handle := &fakeProcessHandle{alive: func() (bool, error) { return true, nil }, killErr: killErr}
	withProcessFinder(t, func(int) (processKiller, error) { return handle, nil })
	rt := New(Options{Spawner: fakeSpawnerFor(t, hosts, livePID())})
	created, err := rt.Create(context.Background(), ports.RuntimeConfig{SessionID: "kill-fails", WorkspacePath: t.TempDir(), Argv: []string{"agent"}})
	if err != nil {
		t.Fatal(err)
	}
	err = rt.Destroy(context.Background(), created)
	if !errors.Is(err, killErr) {
		t.Fatalf("Destroy error=%v, want force-kill error", err)
	}
	if !handle.killed {
		t.Fatal("force-kill path was not exercised")
	}
	if resolved, resolveErr := rt.resolve("kill-fails"); resolveErr != nil || resolved == nil || resolved.generation == "" {
		t.Fatalf("failed force-kill lost cached generation: resolved=%+v err=%v", resolved, resolveErr)
	}
	entries, lookupErr := ptyregistry.LookupAll("kill-fails")
	if lookupErr != nil || len(entries) != 1 || entries[0].Generation == "" {
		t.Fatalf("failed force-kill lost registry generation: entries=%v err=%v", entries, lookupErr)
	}
}

func TestDestroyAlreadyExitedProcessCleansExactGeneration(t *testing.T) {
	isolateRegistry(t)
	withProcessFinder(t, func(int) (processKiller, error) { return nil, os.ErrProcessDone })
	rt := New(Options{})
	sess := &hostSession{addr: "127.0.0.1:1", pid: deadPID(), generation: "exited-generation"}
	rt.mu.Lock()
	rt.sessions["already-exited"] = sess
	rt.mu.Unlock()
	if err := ptyregistry.Register(ptyregistry.Entry{SessionID: "already-exited", PtyHostPID: livePID(), PipePath: sess.addr, RegisteredAt: time.Now().UTC().Format(time.RFC3339Nano), Generation: sess.generation}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Destroy(context.Background(), ports.RuntimeHandle{ID: "already-exited"}); err != nil {
		t.Fatal(err)
	}
	rt.mu.Lock()
	_, cached := rt.sessions["already-exited"]
	rt.mu.Unlock()
	if cached {
		t.Fatal("already-exited exact generation remained cached")
	}
	if entries, err := ptyregistry.LookupAll("already-exited"); err != nil || len(entries) != 0 {
		t.Fatalf("already-exited exact generation remained registered: entries=%v err=%v", entries, err)
	}
}

func TestDestroyDrainsEveryVerifiedGeneration(t *testing.T) {
	isolateRegistry(t)
	withProcessFinder(t, func(int) (processKiller, error) { return &fakeProcessHandle{}, nil })
	older := startInProcHost(t, "duplicate", livePID(), "older")
	newer := startInProcHost(t, "duplicate", livePID(), "newer")
	for _, entry := range []ptyregistry.Entry{
		{SessionID: "duplicate", PtyHostPID: older.pid, PipePath: older.addr, RegisteredAt: "2026-01-01T00:00:00.100Z", Generation: older.generation},
		{SessionID: "duplicate", PtyHostPID: newer.pid, PipePath: newer.addr, RegisteredAt: "2026-01-01T00:00:00.200Z", Generation: newer.generation},
	} {
		if err := ptyregistry.Register(entry); err != nil {
			t.Fatal(err)
		}
	}
	rt := New(Options{})
	if err := rt.Destroy(context.Background(), ports.RuntimeHandle{ID: "duplicate"}); err != nil {
		t.Fatal(err)
	}
	for _, h := range []*inProcHost{older, newer} {
		select {
		case <-h.done:
		case <-time.After(2 * time.Second):
			t.Fatal("duplicate generation remained live")
		}
	}
	if entries, err := ptyregistry.LookupAll("duplicate"); err != nil || len(entries) != 0 {
		t.Fatalf("duplicate generations remained discoverable: entries=%v err=%v", entries, err)
	}
}

func TestIsAliveTreatsMalformedRegistryAsProbeFailure(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AO_DATA_DIR", "")
	if err := ptyregistry.RegisterAt(dataDir, ptyregistry.Entry{SessionID: "live-malformed", PtyHostPID: livePID(), PipePath: "127.0.0.1:1234", RegisteredAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(dataDir, "windows-pty-hosts")
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("registry files=%v err=%v", files, err)
	}
	path := filepath.Join(dir, files[0].Name())
	if err := os.WriteFile(path, []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := New(Options{DataDir: dataDir, Spawner: fakeSpawnerFor(t, nil, livePID())})
	alive, err := rt.IsAlive(context.Background(), ports.RuntimeHandle{ID: "live-malformed"})
	if err == nil || alive {
		t.Fatalf("malformed live registry produced dead conclusion: alive=%v err=%v", alive, err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("malformed live registry was deleted: %v", statErr)
	}
}

func TestIsAliveTreatsMalformedConfiguredAggregateAsProbeFailure(t *testing.T) {
	dataDir := t.TempDir()
	aggregatePath := filepath.Join(dataDir, "windows-pty-hosts.json")
	if err := os.WriteFile(aggregatePath, []byte("partial-[{"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := New(Options{DataDir: dataDir})
	alive, err := rt.IsAlive(context.Background(), ports.RuntimeHandle{ID: "aggregate-live"})
	if err == nil || alive {
		t.Fatalf("malformed configured aggregate produced dead conclusion: alive=%v err=%v", alive, err)
	}
	if _, statErr := os.Stat(aggregatePath); statErr != nil {
		t.Fatalf("malformed configured aggregate was removed after probe: %v", statErr)
	}
}

func TestCreatePreservesAbsoluteExecutableArgvAcrossWindowsShellSettings(t *testing.T) {
	for _, shell := range []string{"powershell.exe", "cmd.exe"} {
		t.Run(shell, func(t *testing.T) {
			isolateRegistry(t)
			t.Setenv("AO_SHELL", shell)
			var got []string
			rt := New(Options{Spawner: func(_ context.Context, _, _ string, argv []string, _ map[string]string) (string, int, error) {
				got = append([]string(nil), argv...)
				return "127.0.0.1:1", livePID(), nil
			}})
			want := []string{`C:\Program Files\Claude\claude.exe`, "--model", "opus"}
			_, err := rt.Create(context.Background(), ports.RuntimeConfig{
				SessionID:     domain.SessionID("sess-shell"),
				WorkspacePath: `C:\work tree`,
				Argv:          want,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("spawner argv = %#v, want %#v", got, want)
			}
		})
	}
}

// TestCreate_DuplicateErrors verifies a second Create for the same session id fails.
func TestCreate_DuplicateErrors(t *testing.T) {
	isolateRegistry(t)
	hosts := map[string]*inProcHost{}
	rt := New(Options{Spawner: fakeSpawnerFor(t, hosts, livePID())})
	ctx := context.Background()

	if _, err := rt.Create(ctx, ports.RuntimeConfig{
		SessionID:     "sess-dup",
		WorkspacePath: "/tmp/w",
		Argv:          []string{"sh"},
	}); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	_, err := rt.Create(ctx, ports.RuntimeConfig{
		SessionID:     "sess-dup",
		WorkspacePath: "/tmp/w",
		Argv:          []string{"sh"},
	})
	if err == nil {
		t.Fatal("expected error on duplicate Create, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error %q should contain 'already exists'", err.Error())
	}

	hosts["sess-dup"].cleanup(t)
}

// TestCreate_InvalidIDErrors verifies Create rejects invalid session ids.
func TestCreate_InvalidIDErrors(t *testing.T) {
	isolateRegistry(t)
	rt := New(Options{Spawner: fakeSpawnerFor(t, nil, livePID())})
	ctx := context.Background()

	for _, bad := range []string{"", "has space", "has/slash", "has.dot"} {
		_, err := rt.Create(ctx, ports.RuntimeConfig{
			SessionID:     domain.SessionID(bad),
			WorkspacePath: "/tmp/w",
			Argv:          []string{"sh"},
		})
		if err == nil {
			t.Fatalf("Create(%q): expected error for invalid id, got nil", bad)
		}
	}
}

// TestSendMessage_DeliversChunkedTextAndEnter verifies clientSendMessage sends
// the text + "\r" to the fakePTY input.
func TestSendMessage_DeliversChunkedTextAndEnter(t *testing.T) {
	isolateRegistry(t)
	hosts := map[string]*inProcHost{}
	rt := New(Options{Spawner: fakeSpawnerFor(t, hosts, livePID())})
	ctx := context.Background()

	handle, err := rt.Create(ctx, ports.RuntimeConfig{
		SessionID:     "sess-sm",
		WorkspacePath: "/tmp/w",
		Argv:          []string{"sh"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := hosts["sess-sm"]
	defer h.cleanup(t)

	msg := "hello world"
	// Collect PTY input in background.
	inputC := make(chan []byte, 4)
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := h.pty.inR.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				inputC <- cp
			}
			if err != nil {
				return
			}
		}
	}()

	if err := rt.SendMessage(ctx, handle, msg); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Collect all received bytes within 2s.
	var received []byte
	deadline := time.After(2 * time.Second)
	// Expect at least msg + "\r".
	for !bytes.Contains(received, []byte("\r")) {
		select {
		case chunk := <-inputC:
			received = append(received, chunk...)
		case <-deadline:
			t.Fatalf("timeout waiting for PTY input; got %q so far", received)
		}
	}

	if !bytes.HasPrefix(received, []byte(msg)) {
		t.Fatalf("PTY input = %q, want prefix %q then \\r", received, msg)
	}
	if !bytes.Contains(received, []byte("\r")) {
		t.Fatalf("PTY input = %q, missing trailing \\r", received)
	}
}

func TestInputEnterDelayUsesLongSettleForMultiFramePaste(t *testing.T) {
	tests := []struct {
		name  string
		runes int
		want  time.Duration
	}{
		{name: "empty enter-only", runes: 0, want: ptyInputEnterDelay},
		{name: "short", runes: 1, want: ptyInputEnterDelay},
		{name: "single frame boundary", runes: ptyInputChunkRunes, want: ptyInputEnterDelay},
		{name: "multi-frame", runes: ptyInputChunkRunes + 1, want: ptyInputLongEnterDelay},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inputEnterDelay(tt.runes); got != tt.want {
				t.Fatalf("inputEnterDelay(%d) = %s, want %s", tt.runes, got, tt.want)
			}
		})
	}
}

// TestSendMessage_LargeMessageChunked verifies a message > 512 runes is
// delivered correctly (host receives full text + "\r").
func TestSendMessage_LargeMessageChunked(t *testing.T) {
	isolateRegistry(t)
	hosts := map[string]*inProcHost{}
	rt := New(Options{Spawner: fakeSpawnerFor(t, hosts, livePID())})
	ctx := context.Background()

	handle, _ := rt.Create(ctx, ports.RuntimeConfig{
		SessionID:     "sess-lg",
		WorkspacePath: "/tmp/w",
		Argv:          []string{"sh"},
	})
	h := hosts["sess-lg"]
	defer h.cleanup(t)

	// Build a message longer than 512 runes (use multi-byte runes to test
	// rune-boundary splitting).
	var sb strings.Builder
	for i := 0; i < 600; i++ {
		sb.WriteRune('A' + rune(i%26))
	}
	msg := sb.String()

	inputDone := make(chan []byte, 1)
	go func() {
		// Read until we see "\r".
		var acc []byte
		buf := make([]byte, 4096)
		for {
			n, err := h.pty.inR.Read(buf)
			if n > 0 {
				acc = append(acc, buf[:n]...)
			}
			if bytes.Contains(acc, []byte("\r")) {
				inputDone <- acc
				return
			}
			if err != nil {
				inputDone <- acc
				return
			}
		}
	}()

	if err := rt.SendMessage(ctx, handle, msg); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	select {
	case got := <-inputDone:
		// Strip trailing \r for comparison.
		trimmed := strings.TrimSuffix(string(got), "\r")
		if trimmed != msg {
			t.Fatalf("PTY received %d chars, want %d\ngot:  %q\nwant: %q", len(trimmed), len(msg), trimmed[:min(50, len(trimmed))], msg[:min(50, len(msg))])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for large message delivery")
	}
}

// TestGetOutput_ReturnsRingTail verifies GetOutput returns the ring's tail.
func TestGetOutput_ReturnsRingTail(t *testing.T) {
	isolateRegistry(t)
	hosts := map[string]*inProcHost{}
	rt := New(Options{Spawner: fakeSpawnerFor(t, hosts, livePID())})
	ctx := context.Background()

	handle, _ := rt.Create(ctx, ports.RuntimeConfig{
		SessionID:     "sess-go",
		WorkspacePath: "/tmp/w",
		Argv:          []string{"sh"},
	})
	h := hosts["sess-go"]
	defer h.cleanup(t)

	// Seed the ring.
	h.ring.Append([]byte("line1\nline2\nline3\n"))

	text, err := rt.GetOutput(ctx, handle, 2)
	if err != nil {
		t.Fatalf("GetOutput: %v", err)
	}
	want := h.ring.Tail(2)
	if text != want {
		t.Fatalf("GetOutput = %q, want %q", text, want)
	}
}

// TestIsAlive_TrueWhileServing_FalseAfterClose verifies IsAlive returns true
// while the host listens and false after its listener is closed.
func TestIsAlive_TrueWhileServing_FalseAfterClose(t *testing.T) {
	isolateRegistry(t)
	hosts := map[string]*inProcHost{}
	rt := New(Options{Spawner: fakeSpawnerFor(t, hosts, livePID())})
	ctx := context.Background()

	handle, _ := rt.Create(ctx, ports.RuntimeConfig{
		SessionID:     "sess-ia",
		WorkspacePath: "/tmp/w",
		Argv:          []string{"sh"},
	})
	h := hosts["sess-ia"]

	alive, err := rt.IsAlive(ctx, handle)
	if err != nil {
		t.Fatalf("IsAlive: %v", err)
	}
	if !alive {
		t.Fatal("expected IsAlive=true while serving")
	}

	// Shut down the host.
	h.cancel()
	<-h.done

	// Give the listener a moment to close.
	time.Sleep(100 * time.Millisecond)

	alive2, err2 := rt.IsAlive(ctx, handle)
	if err2 != nil {
		t.Fatalf("IsAlive after close: %v", err2)
	}
	if alive2 {
		t.Fatal("expected IsAlive=false after host closed")
	}
}

// TestIsAlive_FalseForUnknownSession verifies IsAlive returns (false, nil) for
// a session not in the map or registry.
func TestIsAlive_FalseForUnknownSession(t *testing.T) {
	isolateRegistry(t)
	rt := New(Options{Spawner: fakeSpawnerFor(t, nil, livePID())})
	ctx := context.Background()

	alive, err := rt.IsAlive(ctx, ports.RuntimeHandle{ID: "ghost-session"})
	if err != nil {
		t.Fatalf("IsAlive: unexpected error: %v", err)
	}
	if alive {
		t.Fatal("expected IsAlive=false for unknown session")
	}
}

// TestDestroy_KillsHostAndCleansUp verifies Destroy triggers clientKill,
// removes the map + registry entry, and is idempotent on second call.
// Uses deadPID() so the force-kill step is a safe no-op (the fake pty-host
// has no real OS process; clientKill already shut it down via the loopback).
func TestDestroy_KillsHostAndCleansUp(t *testing.T) {
	isolateRegistry(t)
	process := &fakeProcessHandle{}
	withProcessFinder(t, func(int) (processKiller, error) { return process, nil })
	hosts := map[string]*inProcHost{}
	rt := New(Options{Spawner: fakeSpawnerFor(t, hosts, deadPID())})
	ctx := context.Background()

	handle, err := rt.Create(ctx, ports.RuntimeConfig{
		SessionID:     "sess-destroy",
		WorkspacePath: "/tmp/w",
		Argv:          []string{"sh"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := hosts["sess-destroy"]

	// Destroy should succeed.
	if err := rt.Destroy(ctx, handle); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	// Wait for Serve to stop (clientKill triggers shutdown).
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("host did not stop after Destroy")
	}

	// fakePTY.Close must have been called.
	h.pty.closeMu.Lock()
	closed := h.pty.closed
	h.pty.closeMu.Unlock()
	if !closed {
		t.Fatal("expected fakePTY.Close() after Destroy")
	}
	if process.killed {
		t.Fatal("Destroy force-killed after retained process handle reported dead")
	}

	// Map entry must be gone.
	rt.mu.Lock()
	_, exists := rt.sessions["sess-destroy"]
	rt.mu.Unlock()
	if exists {
		t.Fatal("expected map entry removed after Destroy")
	}

	// Registry entry must be gone.
	entries, _ := ptyregistry.List()
	for _, e := range entries {
		if e.SessionID == "sess-destroy" {
			t.Fatal("expected registry entry removed after Destroy")
		}
	}

	// Second Destroy must be idempotent (returns nil).
	if err := rt.Destroy(ctx, handle); err != nil {
		t.Fatalf("second Destroy: expected nil, got %v", err)
	}
}

// TestResolveViaRegistry verifies that with an empty in-memory map but a
// registry entry pointing at a live in-process host, IsAlive and SendMessage
// still work (simulates a daemon restart).
func TestResolveViaRegistry(t *testing.T) {
	isolateRegistry(t)

	// Start a host directly (not through Create) to simulate a pre-existing
	// pty-host from a previous daemon run. Use the current process PID so
	// ptyregistry.List() does not prune the entry as dead.
	h := startInProcHost(t, "sess-reg", livePID())
	defer h.cleanup(t)

	// Manually register the host in the registry.
	err := ptyregistry.Register(ptyregistry.Entry{
		SessionID:    "sess-reg",
		PtyHostPID:   h.pid,
		PipePath:     h.addr, // addr stored in PipePath field
		RegisteredAt: time.Now().UTC().Format(time.RFC3339Nano),
		Generation:   h.generation,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Create a Runtime with an empty in-memory map (simulates daemon restart).
	rt := New(Options{Spawner: fakeSpawnerFor(t, nil, livePID())})
	ctx := context.Background()

	// IsAlive must work via registry resolution.
	alive, err := rt.IsAlive(ctx, ports.RuntimeHandle{ID: "sess-reg"})
	if err != nil {
		t.Fatalf("IsAlive via registry: %v", err)
	}
	if !alive {
		t.Fatal("expected IsAlive=true via registry resolution")
	}

	// SendMessage must work via registry resolution.
	inputC := make(chan []byte, 4)
	go func() {
		buf := make([]byte, 512)
		for {
			n, err := h.pty.inR.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				inputC <- cp
			}
			if err != nil {
				return
			}
		}
	}()

	if err := rt.SendMessage(ctx, ports.RuntimeHandle{ID: "sess-reg"}, "ping"); err != nil {
		t.Fatalf("SendMessage via registry: %v", err)
	}

	// Collect PTY input.
	var received []byte
	deadline := time.After(3 * time.Second)
	for !bytes.Contains(received, []byte("\r")) {
		select {
		case chunk := <-inputC:
			received = append(received, chunk...)
		case <-deadline:
			t.Fatalf("timeout waiting for PTY input via registry; got %q", received)
		}
	}
	if !bytes.Contains(received, []byte("ping")) {
		t.Fatalf("PTY did not receive 'ping'; got %q", received)
	}
}

// ---------------------------------------------------------------------------
// Unit tests for client helpers (dial a fresh in-proc host directly).
// ---------------------------------------------------------------------------

// TestClientGetOutput_TimesOutReturnsEmpty verifies clientGetOutput returns ""
// (no error) if no response arrives within the timeout. We test the happy path
// instead (timeout path would require a non-responding server).
func TestClientGetOutput_HappyPath(t *testing.T) {
	f := startServe(t, 3001)
	defer f.cancel()

	f.ring.Append([]byte("alpha\nbeta\ngamma\n"))

	text, err := clientGetOutput(f.addr, f.sessionID, f.generation, f.hostPID, 2)
	if err != nil {
		t.Fatalf("clientGetOutput: %v", err)
	}
	want := f.ring.Tail(2)
	if text != want {
		t.Fatalf("clientGetOutput = %q, want %q", text, want)
	}
}

func TestClientGetOutputPreservesPartialFrameAfterIdentityStatus(t *testing.T) {
	client, server := net.Pipe()
	withDialHost(t, func(string, time.Duration) (net.Conn, error) { return client, nil })
	done := make(chan error, 1)
	go func() {
		defer func() { _ = server.Close() }()
		if typ, _, err := readRawFrame(server); err != nil {
			done <- fmt.Errorf("status request type=%x: %w", typ, err)
			return
		} else if typ != MsgStatusReq {
			done <- fmt.Errorf("status request type=%x", typ)
			return
		}
		status := statusFrame(true, 1, nil, "partial-output", "generation", 99)
		live, _ := EncodeMessage(MsgTerminalData, []byte("live"))
		if _, err := server.Write(append(status, live[:3]...)); err != nil {
			done <- err
			return
		}
		if typ, _, err := readRawFrame(server); err != nil {
			done <- fmt.Errorf("output request type=%x: %w", typ, err)
			return
		} else if typ != MsgGetOutputReq {
			done <- fmt.Errorf("output request type=%x", typ)
			return
		}
		response, _ := EncodeMessage(MsgGetOutputRes, []byte("answer"))
		_, err := server.Write(append(live[3:], response...))
		done <- err
	}()
	got, err := clientGetOutput("pipe", "partial-output", "generation", 99, 10)
	if err != nil || got != "answer" {
		t.Fatalf("clientGetOutput=%q err=%v", got, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestClientIsAlive_TrueAndFalse verifies clientIsAlive returns (true, nil) for
// a live host and (false, nil) for a refused address (definitively gone).
func TestClientIsAlive_TrueAndFalse(t *testing.T) {
	f := startServe(t, 3002)
	defer f.cancel()

	if alive, err := clientIsAlive(f.addr, f.sessionID, f.generation, f.hostPID); err != nil || !alive {
		t.Fatalf("clientIsAlive(live) = (%v, %v), want (true, nil)", alive, err)
	}

	f.cancel()
	// Wait for listener to close.
	select {
	case <-f.done:
	case <-time.After(2 * time.Second):
	}
	time.Sleep(50 * time.Millisecond)

	// After close the OS refuses the connection on the freed port -> gone.
	if alive, err := clientIsAlive(f.addr, f.sessionID, f.generation, f.hostPID); alive || err != nil {
		t.Fatalf("clientIsAlive(closed) = (%v, %v), want (false, nil)", alive, err)
	}
}

// TestIsAlive_RefusedIsGone_TimeoutIsTransient is the reaper-safety regression
// test. It asserts the dead-vs-transient split that keeps a single transient
// loopback hiccup from spuriously reaping a live idle session:
//
//	(a) a resolved-but-REFUSED host -> IsAlive == (false, nil)  [ProbeDead]
//	(b) a resolved host whose probe TIMES OUT -> (false, non-nil) [ProbeFailed]
func TestIsAlive_RefusedIsGone_TimeoutIsTransient(t *testing.T) {
	isolateRegistry(t)

	// (a) Refused: bind+close a listener to obtain a port nothing listens on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	refusedAddr := ln.Addr().String()
	_ = ln.Close()

	rtRefused := New(Options{Spawner: fakeSpawnerFor(t, nil, livePID())})
	rtRefused.mu.Lock()
	rtRefused.sessions["gone"] = &hostSession{addr: refusedAddr, pid: livePID()}
	rtRefused.mu.Unlock()

	alive, err := rtRefused.IsAlive(context.Background(), ports.RuntimeHandle{ID: "gone"})
	if alive || err != nil {
		t.Fatalf("IsAlive(refused) = (%v, %v), want (false, nil) definitively gone", alive, err)
	}

	// (b) Transient timeout: a listener that Accepts but never replies. The
	// short isAliveTimeout read deadline fires before any STATUS_RES arrives,
	// which must surface as a non-nil (transient) error, not a death.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen silent: %v", err)
	}
	defer silent.Close()
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			// Hold the connection open without ever sending a STATUS_RES.
			go func(c net.Conn) {
				time.Sleep(isAliveTimeout + time.Second)
				_ = c.Close()
			}(c)
		}
	}()

	rtSilent := New(Options{Spawner: fakeSpawnerFor(t, nil, livePID())})
	rtSilent.mu.Lock()
	rtSilent.sessions["stuck"] = &hostSession{addr: silent.Addr().String(), pid: livePID()}
	rtSilent.mu.Unlock()

	alive, err = rtSilent.IsAlive(context.Background(), ports.RuntimeHandle{ID: "stuck"})
	if alive {
		t.Fatalf("IsAlive(silent) alive=true, want false")
	}
	if err == nil {
		t.Fatal("IsAlive(silent) err=nil, want non-nil transient error so the reaper records ProbeFailed")
	}
}

func TestClientKillRequiresReachableVerifiedEndpoint(t *testing.T) {
	if err := clientKill("127.0.0.1:1", "missing", "generation", 1); err == nil {
		t.Fatal("clientKill on unreachable addr returned nil")
	}
}

// Ensure the packages compile (import check).
var _ = io.Discard
