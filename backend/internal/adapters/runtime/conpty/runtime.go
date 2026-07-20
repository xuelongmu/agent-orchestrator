// runtime.go - conpty Runtime adapter. Implements ports.Runtime and
// ports.Attacher (see attach.go). Drives sessions via the B3 pty-host over
// loopback TCP, using the B1 protocol and the B2 registry for restart recovery.
package conpty

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/conpty/ptyregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Ensure Runtime satisfies the port at compile time (Attach in attach.go).
var _ ports.Runtime = (*Runtime)(nil)

// validSessionID matches agent-orchestrator's assertValidSessionId.
var validSessionID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

const (
	dataDirEnv        = "AO_DATA_DIR"
	hostGenerationEnv = "AO_PTY_HOST_GENERATION"
)

var errHostLaunchInProgress = errors.New("conpty: host launch is still in progress")

// hostSession is the in-memory state for a live pty-host connection.
type hostSession struct {
	addr       string
	pid        int
	generation string
}

// Options configures the Runtime. All fields are optional; zero values use
// sensible defaults. The Spawner field is injectable for tests.
type Options struct {
	// Spawner overrides the default OS-level process spawner. If nil,
	// defaultSpawnHost is used (Windows-only; returns an error on other OSes).
	Spawner hostSpawner
	// RegistryRegister is injectable for publication-failure tests. Production
	// uses the durable ptyregistry sideband.
	RegistryRegister func(ptyregistry.Entry) error
	// RegistryLookupAll is injectable for focused multi-generation recovery tests.
	RegistryLookupAll func(string) ([]ptyregistry.Entry, error)
	// DataDir pins parent-side register/list/unregister operations to the same
	// namespace exported to the detached pty-host as AO_DATA_DIR.
	DataDir string
}

// Runtime is the conpty runtime adapter.
type Runtime struct {
	spawner    hostSpawner
	register   func(ptyregistry.Entry) error
	unregister func(string, string) error
	lookupAll  func(string) ([]ptyregistry.Entry, error)
	dataDir    string
	initErr    error

	mu       sync.Mutex
	sessions map[string]*hostSession // sessionID -> live session
}

// New creates a Runtime with the given options.
func New(opts Options) *Runtime {
	dataDir := opts.DataDir
	if dataDir == "" {
		dataDir, _ = os.LookupEnv(dataDirEnv)
	}
	var initErr error
	if dataDir != "" {
		configuredDataDir := dataDir
		dataDir, initErr = filepath.Abs(dataDir)
		if initErr != nil {
			initErr = fmt.Errorf("conpty: resolve data dir %q: %w", configuredDataDir, initErr)
		} else {
			dataDir = filepath.Clean(dataDir)
		}
	}
	sp := opts.Spawner
	if sp == nil {
		sp = defaultSpawnHost
	}
	register := opts.RegistryRegister
	if register == nil {
		if dataDir == "" {
			register = ptyregistry.Register
		} else {
			register = func(entry ptyregistry.Entry) error { return ptyregistry.RegisterAt(dataDir, entry) }
		}
	}
	lookupAll := opts.RegistryLookupAll
	if lookupAll == nil {
		if dataDir == "" {
			lookupAll = ptyregistry.LookupAll
		} else {
			lookupAll = func(id string) ([]ptyregistry.Entry, error) { return ptyregistry.LookupAllAt(dataDir, id) }
		}
	}
	unregister := func(id, generation string) error {
		ambientDataDir, _ := os.LookupEnv(dataDirEnv)
		return ptyregistry.UnregisterGenerationAt(ambientDataDir, id, generation)
	}
	if dataDir != "" {
		unregister = func(id, generation string) error {
			return ptyregistry.UnregisterGenerationAt(dataDir, id, generation)
		}
	}
	return &Runtime{
		spawner:    sp,
		register:   register,
		unregister: unregister,
		lookupAll:  lookupAll,
		dataDir:    dataDir,
		initErr:    initErr,
		sessions:   make(map[string]*hostSession),
	}
}

func isReservedHostEnvKey(key string) bool {
	return strings.EqualFold(key, dataDirEnv) || strings.EqualFold(key, hostGenerationEnv)
}

// ExpectedHandle returns Create's deterministic handle before external launch.
func (r *Runtime) ExpectedHandle(id domain.SessionID) ports.RuntimeHandle {
	return ports.RuntimeHandle{ID: string(id)}
}

// Create spawns a detached pty-host for the session, waits for READY, stores
// the addr+pid in-memory and in the B2 registry, and returns the handle.
// Returns an error if sessionID is invalid, already exists, or spawn fails.
func (r *Runtime) Create(ctx context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	if r.initErr != nil {
		return ports.RuntimeHandle{}, r.initErr
	}
	id := string(cfg.SessionID)
	if !validSessionID.MatchString(id) {
		return ports.RuntimeHandle{}, fmt.Errorf("conpty: invalid session id %q: must match ^[a-zA-Z0-9_-]+$", id)
	}
	if cfg.WorkspacePath == "" {
		return ports.RuntimeHandle{}, fmt.Errorf("conpty: workspace path required")
	}
	if len(cfg.Argv) == 0 {
		return ports.RuntimeHandle{}, fmt.Errorf("conpty: argv required")
	}

	r.mu.Lock()
	if _, dup := r.sessions[id]; dup {
		r.mu.Unlock()
		return ports.RuntimeHandle{}, fmt.Errorf("conpty: session %q already exists; destroy before re-creating", id)
	}
	existing, err := r.lookupVerifiedLocked(id)
	if err != nil {
		r.mu.Unlock()
		return ports.RuntimeHandle{}, fmt.Errorf("conpty: preflight existing host for %q: %w", id, err)
	}
	if existing != nil {
		r.sessions[id] = existing
		r.mu.Unlock()
		return ports.RuntimeHandle{}, fmt.Errorf("conpty: session %q already has a live host generation; destroy before re-creating", id)
	}
	// Reserve the slot before the async spawn so a concurrent Create for the
	// same id fails immediately (no gap between check and set).
	r.sessions[id] = nil
	r.mu.Unlock()

	generation, err := newHostGeneration()
	if err != nil {
		r.mu.Lock()
		delete(r.sessions, id)
		r.mu.Unlock()
		return ports.RuntimeHandle{}, fmt.Errorf("conpty: create host generation for %q: %w", id, err)
	}
	spawnEnv := make(map[string]string, len(cfg.Env)+2)
	for key, value := range cfg.Env {
		if isReservedHostEnvKey(key) {
			continue
		}
		spawnEnv[key] = value
	}
	if r.dataDir != "" {
		// RunHost publishes before READY through the ambient registry API. Pin
		// every detached host—including reviewer launches whose caller env is
		// otherwise sparse—to the same explicit namespace used by this Runtime.
		spawnEnv[dataDirEnv] = r.dataDir
	}
	spawnEnv[hostGenerationEnv] = generation
	addr, pid, err := r.spawner(ctx, id, cfg.WorkspacePath, cfg.Argv, spawnEnv)
	if err != nil {
		r.mu.Lock()
		delete(r.sessions, id)
		r.mu.Unlock()
		return ports.RuntimeHandle{}, fmt.Errorf("conpty: spawn pty-host for %q: %w", id, err)
	}

	sess := &hostSession{addr: addr, pid: pid, generation: generation}

	// Publication is authoritative: Create cannot report success unless the
	// detached host is discoverable by a replacement daemon. The host process
	// also publishes before READY, closing the parent-crash boundary.
	if err := r.register(ptyregistry.Entry{
		SessionID:    id,
		PtyHostPID:   pid,
		PipePath:     addr, // ponytail: reuse PipePath field for loopback addr
		RegisteredAt: time.Now().UTC().Format(time.RFC3339Nano),
		Generation:   generation,
	}); err != nil {
		cleanupErr := r.destroySession(id, sess)
		r.mu.Lock()
		if current, exists := r.sessions[id]; exists && current == nil {
			delete(r.sessions, id)
		}
		r.mu.Unlock()
		return ports.RuntimeHandle{}, errors.Join(fmt.Errorf("conpty: publish pty-host %q: %w", id, err), cleanupErr)
	}

	// Publish the in-memory generation only after the durable parent registry
	// write succeeds. Until this commit, resolve/Destroy observe the reserved nil
	// slot and return launch-in-progress rather than racing cleanup with Create.
	r.mu.Lock()
	if current, exists := r.sessions[id]; !exists || current != nil {
		r.mu.Unlock()
		cleanupErr := r.destroySession(id, sess)
		return ports.RuntimeHandle{}, errors.Join(fmt.Errorf("conpty: launch reservation for %q was lost before commit", id), cleanupErr)
	}
	r.sessions[id] = sess
	r.mu.Unlock()

	return ports.RuntimeHandle{ID: id}, nil
}

// Destroy gracefully kills the pty-host, waits up to ~500ms for the pid to
// exit, then force-kills it. Removes the session from the map and the registry.
// Idempotent: unknown/already-gone session returns nil.
func (r *Runtime) Destroy(ctx context.Context, handle ports.RuntimeHandle) error {
	for {
		sess, err := r.resolve(handle.ID)
		if err != nil {
			return fmt.Errorf("conpty: resolve %q for destroy: %w", handle.ID, err)
		}
		if sess == nil {
			return nil // unknown or every generation is gone
		}
		if err := r.destroySession(handle.ID, sess); err != nil {
			return err
		}
	}
}

func (r *Runtime) destroySession(id string, sess *hostSession) error {

	// Open the process object before probing the endpoint. On Windows the
	// retained handle remains bound to that exact process even if it exits and
	// its numeric PID is reused while Destroy is in flight.
	ownedProcess, err := findProcess(sess.pid)
	if err != nil {
		if isProcessNotFound(err) {
			return r.evictGeneration(id, sess)
		}
		return fmt.Errorf("conpty: open host process %q: %w", id, err)
	}
	defer func() { _ = ownedProcess.Close() }()

	// Prove the endpoint, host PID, session, and creation generation while the
	// exact process handle is retained before any destructive action.
	// destructive action. A stale registry PID or reused loopback port must not
	// let session A gracefully or forcibly terminate session B.
	alive, err := clientIsAlive(sess.addr, id, sess.generation, sess.pid)
	if err != nil {
		var mismatch *hostIdentityMismatchError
		if errors.As(err, &mismatch) {
			if cleanupErr := r.evictGeneration(id, sess); cleanupErr != nil {
				return errors.Join(err, cleanupErr)
			}
			return nil
		}
		return fmt.Errorf("conpty: verify %q before destroy: %w", id, err)
	}
	if alive {
		if err := clientKill(sess.addr, id, sess.generation, sess.pid); err != nil {
			return fmt.Errorf("conpty: stop verified host %q: %w", id, err)
		}

		// Poll the retained process object, never the reusable numeric PID.
		stillAlive, err := ownedProcess.Alive()
		if err != nil {
			return fmt.Errorf("conpty: poll verified host %q: %w", id, err)
		}
		deadline := time.Now().Add(500 * time.Millisecond)
		for stillAlive && time.Now().Before(deadline) {
			time.Sleep(25 * time.Millisecond)
			stillAlive, err = ownedProcess.Alive()
			if err != nil {
				return fmt.Errorf("conpty: poll verified host %q: %w", id, err)
			}
		}
		// Never look the PID up after observing it dead. Only a process handle
		// captured while the endpoint identity was proven may be force-killed.
		if stillAlive {
			if err := ownedProcess.Kill(); err != nil {
				return fmt.Errorf("conpty: force-kill verified host %q: %w", id, err)
			}
		}
	}

	// Remove the exact durable generation before exposing an absent map slot.
	// Otherwise a concurrent resolver could read the stale entry and repopulate
	// it after this Destroy completed.
	if err := r.unregister(id, sess.generation); err != nil {
		return fmt.Errorf("conpty: unregister host %q generation %q: %w", id, sess.generation, err)
	}
	r.mu.Lock()
	if r.sessions[id] == sess {
		delete(r.sessions, id)
	}
	r.mu.Unlock()
	return nil
}

// IsAlive distinguishes three outcomes so the reaper never spuriously reaps a
// live session on a transient probe failure:
//
//   - (true, nil):  the pty-host answered a status probe -> alive.
//   - (false, nil): DEFINITIVELY gone. Either the session resolves to nothing
//     (no in-memory entry and no registry entry), or the dial was refused
//     (nothing listening on the loopback addr).
//   - (false, err): a TRANSIENT probe failure (loopback timeout, connected-
//     then-failed I/O). The reaper records ProbeFailed and retries rather than
//     treating it as a death conclusion.
//
// tmux returns a non-nil error for transient failures for the same
// reason; conpty matches that contract here.
func (r *Runtime) IsAlive(ctx context.Context, handle ports.RuntimeHandle) (bool, error) {
	sess, err := r.resolve(handle.ID)
	if err != nil {
		return false, fmt.Errorf("conpty: resolve %q: %w", handle.ID, err)
	}
	if sess == nil {
		return false, nil // no in-memory entry, no registry entry -> definitively gone
	}
	alive, err := clientIsAlive(sess.addr, handle.ID, sess.generation, sess.pid)
	if err != nil {
		var mismatch *hostIdentityMismatchError
		if errors.As(err, &mismatch) {
			if cleanupErr := r.evictGeneration(handle.ID, sess); cleanupErr != nil {
				return false, errors.Join(err, cleanupErr)
			}
			return false, nil
		}
	}
	return alive, err
}

// SendMessage chunks message and writes it to the pty-host followed by Enter.
func (r *Runtime) SendMessage(ctx context.Context, handle ports.RuntimeHandle, message string) error {
	sess, err := r.resolve(handle.ID)
	if err != nil {
		return fmt.Errorf("conpty: resolve %q: %w", handle.ID, err)
	}
	if sess == nil {
		return fmt.Errorf("conpty: session %q not found", handle.ID)
	}
	return r.routeOperationError(handle.ID, sess, clientSendMessage(sess.addr, handle.ID, sess.generation, sess.pid, message))
}

// Interrupt sends Ctrl-C to the PTY without tearing down the terminal host.
func (r *Runtime) Interrupt(ctx context.Context, handle ports.RuntimeHandle) error {
	sess, err := r.resolve(handle.ID)
	if err != nil {
		return fmt.Errorf("conpty: resolve %q: %w", handle.ID, err)
	}
	if sess == nil {
		return fmt.Errorf("conpty: session %q not found", handle.ID)
	}
	return r.routeOperationError(handle.ID, sess, clientSendInput(sess.addr, handle.ID, sess.generation, sess.pid, "\x03"))
}

// GetOutput returns the last lines lines from the pty-host ring buffer.
func (r *Runtime) GetOutput(ctx context.Context, handle ports.RuntimeHandle, lines int) (string, error) {
	if lines <= 0 {
		return "", fmt.Errorf("conpty: lines must be > 0")
	}
	sess, err := r.resolve(handle.ID)
	if err != nil {
		return "", fmt.Errorf("conpty: resolve %q: %w", handle.ID, err)
	}
	if sess == nil {
		return "", fmt.Errorf("conpty: session %q not found", handle.ID)
	}
	output, err := clientGetOutput(sess.addr, handle.ID, sess.generation, sess.pid, lines)
	return output, r.routeOperationError(handle.ID, sess, err)
}

// resolve looks up a session by id: first the in-memory map, then the B2
// registry (for daemon-restart recovery). Returns nil if not found either way.
func (r *Runtime) resolve(id string) (*hostSession, error) {
	if r.initErr != nil {
		return nil, r.initErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, exists := r.sessions[id]
	if exists {
		if sess == nil {
			return nil, errHostLaunchInProgress
		}
		return sess, nil
	}

	recovered, err := r.lookupVerifiedLocked(id)
	if err != nil || recovered == nil {
		return recovered, err
	}
	// Re-populate the map only after the endpoint identity is proven.
	r.sessions[id] = recovered
	return recovered, nil
}

// lookupVerifiedLocked examines a bounded newest-first generation snapshot.
// Proven-stale entries are removed exactly and skipped so an older valid host
// can still be adopted. The caller holds r.mu, serializing lookup/cache with
// Create and completed Destroy inside this Runtime.
func (r *Runtime) lookupVerifiedLocked(id string) (*hostSession, error) {
	entries, err := r.lookupAll(id)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		recovered := &hostSession{addr: e.PipePath, pid: e.PtyHostPID, generation: e.Generation}
		// Never cache or route traffic through a recovered address until the same
		// endpoint proves session, host PID, and generation identity. Missing or
		// malformed legacy identity is a fail-safe probe error; a complete typed
		// mismatch positively proves this exact registry generation stale.
		alive, probeErr := clientIsAlive(recovered.addr, id, recovered.generation, recovered.pid)
		if probeErr != nil {
			var mismatch *hostIdentityMismatchError
			if errors.As(probeErr, &mismatch) {
				if cleanupErr := r.unregister(id, recovered.generation); cleanupErr != nil {
					return nil, errors.Join(probeErr, cleanupErr)
				}
				continue
			}
			return nil, probeErr
		}
		if !alive {
			if err := r.unregister(id, recovered.generation); err != nil {
				return nil, err
			}
			continue
		}
		return recovered, nil
	}
	return nil, nil
}

func (r *Runtime) evictGeneration(id string, sess *hostSession) error {
	if err := r.unregister(id, sess.generation); err != nil {
		return fmt.Errorf("conpty: unregister stale host %q generation %q: %w", id, sess.generation, err)
	}
	r.mu.Lock()
	if r.sessions[id] == sess {
		delete(r.sessions, id)
	}
	r.mu.Unlock()
	return nil
}

func (r *Runtime) routeOperationError(id string, sess *hostSession, err error) error {
	if err == nil {
		return nil
	}
	var mismatch *hostIdentityMismatchError
	if errors.As(err, &mismatch) {
		return errors.Join(err, r.evictGeneration(id, sess))
	}
	return err
}

func newHostGeneration() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// findProcess wraps os.FindProcess to make it swappable in tests.
// ponytail: direct call; no interface needed at this scale.
func findProcess(pid int) (processKiller, error) {
	p, err := osProcessFinder(pid)
	return p, err
}

// processKiller retains an exact process object across identity verification,
// graceful shutdown, and timeout handling. On Windows it wraps a native
// process handle, preventing PID reuse from retargeting Kill.
type processKiller interface {
	Alive() (bool, error)
	Kill() error
	Close() error
}

// osProcessFinder is the production implementation; tests may replace it.
// The real defaultOSProcessFinder is in pidalive_unix.go / pidalive_windows.go
// (same files that provide pidAlive).
var osProcessFinder = defaultOSProcessFinder
