package verification

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// Verification outcomes reported to callers.
const (
	defaultTimeout = 10 * time.Minute
	maxLogBytes    = 1024 * 1024
	retainedLogs   = 10
)

// Store supplies the session workspace and owning project record.
type Store interface {
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
}

// Authorizer validates a capability against its owning session and project.
type Authorizer interface {
	Verify(domain.SessionID, domain.ProjectID, string) bool
}

// RunSpec is the already-validated direct process invocation handed to a Runner.
type RunSpec struct {
	Argv   []string
	Dir    string
	Env    []string
	Output io.Writer
}

// RunResult is the process exit information returned even for a nonzero exit.
type RunResult struct{ ExitCode int }

// Runner owns one verification process tree until it exits or is canceled.
type Runner interface {
	Run(context.Context, RunSpec) (RunResult, error)
}

// Outcome is the user-visible terminal state of a verification run.
type Outcome string

const (
	// OutcomePassed indicates the verification command succeeded.
	OutcomePassed Outcome = "passed"
	// OutcomeFailed indicates the verification command returned failure.
	OutcomeFailed Outcome = "failed"
	// OutcomeCanceled indicates the caller or daemon canceled verification.
	OutcomeCanceled Outcome = "canceled"
	// OutcomeTimedOut indicates verification exceeded its configured timeout.
	OutcomeTimedOut Outcome = "timed_out"
)

// Result reports a completed verification and its bounded log location.
type Result struct {
	SessionID  domain.SessionID `json:"sessionId"`
	Profile    string           `json:"profile"`
	Outcome    Outcome          `json:"outcome" enum:"passed,failed,canceled,timed_out"`
	ExitCode   int              `json:"exitCode"`
	LogPath    string           `json:"logPath"`
	Truncated  bool             `json:"truncated"`
	DurationMS int64            `json:"durationMs"`
	Error      string           `json:"error,omitempty"`
}

// Service resolves allowlisted profiles and owns active workspace runs.
type Service struct {
	store   Store
	runner  Runner
	root    context.Context
	now     func() time.Time
	policy  Policy
	auth    Authorizer
	dataDir string
	mu      sync.Mutex
	active  map[string]struct{}
}

// Deps supplies Service collaborators and the daemon root context.
type Deps struct {
	Store   Store
	Runner  Runner
	Root    context.Context
	Now     func() time.Time
	Policy  Policy
	Auth    Authorizer
	DataDir string
}

// New builds a workspace verification service.
func New(d Deps) *Service {
	root := d.Root
	if root == nil {
		root = context.Background()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	runner := d.Runner
	if runner == nil {
		runner = OSRunner{}
	}
	policy := d.Policy.withDefaults()
	return &Service{store: d.Store, runner: runner, root: root, now: now, policy: policy, auth: d.Auth, dataDir: d.DataDir, active: make(map[string]struct{})}
}

// Run executes one configured profile and waits for its terminal outcome.
func (s *Service) Run(ctx context.Context, sessionID domain.SessionID, profile, capability string) (Result, error) {
	profile = strings.TrimSpace(profile)
	if sessionID == "" {
		return Result{}, apierr.Invalid("SESSION_REQUIRED", "sessionId is required", nil)
	}
	if profile == "" {
		return Result{}, apierr.Invalid("VERIFY_PROFILE_REQUIRED", "verification profile is required", nil)
	}

	session, ok, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return Result{}, apierr.Internal("SESSION_LOAD_FAILED", "Failed to load session")
	}
	if !ok || s.auth == nil || !s.auth.Verify(session.ID, session.ProjectID, capability) {
		return Result{}, apierr.Forbidden("VERIFY_CAPABILITY_INVALID", "Verification capability does not own this session")
	}
	if session.IsTerminated {
		return Result{}, apierr.Conflict("SESSION_TERMINATED", "Cannot verify a terminated session", nil)
	}
	if strings.TrimSpace(session.Metadata.WorkspacePath) == "" {
		return Result{}, apierr.Conflict("WORKSPACE_UNAVAILABLE", "Session workspace is unavailable", nil)
	}
	project, ok, err := s.store.GetProject(ctx, string(session.ProjectID))
	if err != nil {
		return Result{}, apierr.Internal("PROJECT_LOAD_FAILED", "Failed to load project")
	}
	if !ok || !project.ArchivedAt.IsZero() {
		return Result{}, apierr.NotFound("PROJECT_NOT_FOUND", "Unknown project")
	}
	command, ok := s.policy.Resolve(session.ProjectID, profile)
	if !ok {
		return Result{}, apierr.Invalid("VERIFY_PROFILE_NOT_ALLOWED", "Unknown verification profile", map[string]any{"profile": profile, "allowed": s.policy.Allowed(session.ProjectID)})
	}

	workspace, err := confinedWorkspace(session.Metadata.WorkspacePath)
	if err != nil {
		return Result{}, apierr.Conflict("WORKSPACE_UNSAFE", err.Error(), nil)
	}
	workingDir, err := confinedWorkingDirectory(workspace, command.WorkingDirectory)
	if err != nil {
		return Result{}, apierr.Invalid("VERIFY_WORKING_DIRECTORY_INVALID", err.Error(), nil)
	}

	if !s.claim(workspace) {
		return Result{}, apierr.Conflict("VERIFY_ALREADY_RUNNING", "A verification run is already active in this workspace", nil)
	}
	defer s.release(workspace)

	if strings.TrimSpace(s.dataDir) == "" {
		return Result{}, apierr.Internal("VERIFY_LOG_ROOT_UNAVAILABLE", "Verification log storage is unavailable")
	}
	logFile, logPath, err := newLogAt(s.dataDir, string(sessionID))
	if err != nil {
		return Result{}, apierr.Conflict("VERIFY_LOG_UNSAFE", err.Error(), nil)
	}
	defer func() { _ = logFile.Close() }()
	writer := newBoundedWriter(logFile, maxLogBytes)
	start := s.now()
	_, _ = fmt.Fprintf(writer, "AO verification profile %s\ncommand: %s\n\n", profile, formatArgv(command.Argv))

	timeout := defaultTimeout
	if command.TimeoutSeconds > 0 {
		timeout = time.Duration(command.TimeoutSeconds) * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	stopRoot := context.AfterFunc(s.root, cancel)
	defer func() { stopRoot(); cancel() }()

	rr, runErr := s.runner.Run(runCtx, RunSpec{Argv: append([]string(nil), command.Argv...), Dir: workingDir, Env: append(os.Environ(), "AO_VERIFY=1"), Output: writer})
	result := Result{SessionID: sessionID, Profile: profile, ExitCode: rr.ExitCode, LogPath: logPath, Truncated: writer.Truncated(), DurationMS: s.now().Sub(start).Milliseconds()}
	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		result.Outcome, result.Error = OutcomeTimedOut, "verification timed out"
	case errors.Is(runCtx.Err(), context.Canceled):
		result.Outcome, result.Error = OutcomeCanceled, "verification canceled"
	case runErr != nil:
		result.Outcome, result.Error = OutcomeFailed, runErr.Error()
	case rr.ExitCode != 0:
		result.Outcome = OutcomeFailed
	default:
		result.Outcome = OutcomePassed
	}
	if result.Outcome != OutcomePassed {
		_, _ = fmt.Fprintf(writer, "\nAO verification outcome: %s", result.Outcome)
		if result.Error != "" {
			_, _ = fmt.Fprintf(writer, ": %s", result.Error)
		}
		_, _ = io.WriteString(writer, "\n")
		result.Truncated = writer.Truncated()
	}
	return result, nil
}

func (s *Service) claim(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(filepath.Clean(path))
	if _, ok := s.active[key]; ok {
		return false
	}
	s.active[key] = struct{}{}
	return true
}
func (s *Service) release(path string) {
	s.mu.Lock()
	delete(s.active, strings.ToLower(filepath.Clean(path)))
	s.mu.Unlock()
}

func confinedWorkspace(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("workspace is not a directory")
	}
	return filepath.Clean(resolved), nil
}

func confinedWorkingDirectory(workspace, rel string) (string, error) {
	candidate := workspace
	if strings.TrimSpace(rel) != "" {
		candidate = filepath.Join(workspace, filepath.FromSlash(rel))
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("working directory does not exist: %w", err)
	}
	if !pathWithin(workspace, resolved) {
		return "", fmt.Errorf("working directory escapes the session workspace")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("working directory is not a directory")
	}
	return resolved, nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

var logNameRE = regexp.MustCompile(`^verify-(\d+)\.log$`)

func newLogAt(base, scope string) (*os.File, string, error) {
	base, err := filepath.Abs(base)
	if err != nil {
		return nil, "", fmt.Errorf("resolve verification log root: %w", err)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, "", fmt.Errorf("open verification log root: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir("verification", 0o700); err != nil && !os.IsExist(err) {
		return nil, "", fmt.Errorf("create verification root: %w", err)
	}
	verificationRoot, err := root.OpenRoot("verification")
	if err != nil {
		return nil, "", fmt.Errorf("open verification root: %w", err)
	}
	defer func() { _ = verificationRoot.Close() }()
	scopeHash := sha256.Sum256([]byte(scope))
	scopeDir := fmt.Sprintf("session-%x", scopeHash[:12])
	if err := verificationRoot.Mkdir(scopeDir, 0o700); err != nil && !os.IsExist(err) {
		return nil, "", fmt.Errorf("create session verification root: %w", err)
	}
	dirRoot, err := verificationRoot.OpenRoot(scopeDir)
	if err != nil {
		return nil, "", fmt.Errorf("open session verification root: %w", err)
	}
	defer func() { _ = dirRoot.Close() }()
	dirFile, err := dirRoot.Open(".")
	if err != nil {
		return nil, "", fmt.Errorf("open log directory: %w", err)
	}
	entries, err := dirFile.ReadDir(-1)
	_ = dirFile.Close()
	if err != nil {
		return nil, "", fmt.Errorf("list log directory: %w", err)
	}
	highest := 0
	type oldLog struct {
		n    int
		name string
	}
	var logs []oldLog
	for _, entry := range entries {
		m := logNameRE.FindStringSubmatch(entry.Name())
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n > highest {
			highest = n
		}
		if entry.Type().IsRegular() {
			logs = append(logs, oldLog{n, entry.Name()})
		}
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].n < logs[j].n })
	for len(logs) >= retainedLogs {
		if err := dirRoot.Remove(logs[0].name); err != nil {
			return nil, "", fmt.Errorf("prune old verification log: %w", err)
		}
		logs = logs[1:]
	}
	name := fmt.Sprintf("verify-%d.log", highest+1)
	path := filepath.Join(base, "verification", scopeDir, name)
	f, err := dirRoot.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("create verification log: %w", err)
	}
	return f, path, nil
}

func formatArgv(argv []string) string {
	parts := make([]string, len(argv))
	for i, arg := range argv {
		parts[i] = strconv.Quote(arg)
	}
	return strings.Join(parts, " ")
}

type boundedWriter struct {
	mu        sync.Mutex
	file      *os.File
	limit     int64
	truncated bool
}

func newBoundedWriter(f *os.File, limit int64) *boundedWriter {
	return &boundedWriter{file: f, limit: limit}
}
func (w *boundedWriter) Truncated() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.truncated }
func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	original := len(p)
	if int64(len(p)) >= w.limit {
		p = p[len(p)-int(w.limit):]
		w.truncated = true
		if _, err := w.file.Seek(0, 0); err != nil {
			return 0, err
		}
		if _, err := w.file.Write(p); err != nil {
			return 0, err
		}
		if err := w.file.Truncate(w.limit); err != nil {
			return 0, err
		}
		return original, nil
	}
	info, err := w.file.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size()+int64(len(p)) <= w.limit {
		if _, err = w.file.Seek(0, io.SeekEnd); err != nil {
			return 0, err
		}
		if _, err = w.file.Write(p); err != nil {
			return 0, err
		}
		return original, nil
	}
	keep := int(w.limit) - len(p)
	tail := make([]byte, keep)
	if _, err = w.file.ReadAt(tail, info.Size()-int64(keep)); err != nil {
		return 0, err
	}
	if _, err = w.file.Seek(0, 0); err != nil {
		return 0, err
	}
	if _, err = w.file.Write(tail); err != nil {
		return 0, err
	}
	if _, err = w.file.Write(p); err != nil {
		return 0, err
	}
	if err := w.file.Truncate(w.limit); err != nil {
		return 0, err
	}
	w.truncated = true
	return original, nil
}
