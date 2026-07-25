//go:build windows

package process

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	jobObjectAssignProcess = 0x0001
	jobObjectQuery         = 0x0004
	jobObjectTerminate     = 0x0008
	jobObjectSynchronize   = 0x00100000
)

var (
	// ErrSessionJobNotFound means no live pty-host owns the named session job.
	ErrSessionJobNotFound = errors.New("session job not found")
	openJobObjectW        = windows.NewLazySystemDLL("kernel32.dll").NewProc("OpenJobObjectW")
)

// SessionJob is a named Windows Job Object that owns every process belonging
// to one AO session. The pty-host holds the creating handle for its lifetime;
// closing the last handle kills every remaining member.
type SessionJob struct {
	mu     sync.Mutex
	handle windows.Handle
}

// CreateSessionJob creates or opens the named owner job for one exact pty-host
// generation. A recycled session ID can never share ownership with its
// predecessor.
func CreateSessionJob(dataDir, sessionID, generation string) (*SessionJob, error) {
	name, err := sessionJobName(dataDir, sessionID, generation)
	if err != nil {
		return nil, err
	}
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("encode session job name: %w", err)
	}
	handle, err := windows.CreateJobObject(nil, namePtr)
	if err != nil {
		return nil, fmt.Errorf("create session job: %w", err)
	}
	job := &SessionJob{handle: handle}
	if err := job.enableKillOnClose(); err != nil {
		_ = job.Close()
		return nil, err
	}
	return job, nil
}

// OpenSessionJob opens the job owned by one exact running pty-host generation.
func OpenSessionJob(dataDir, sessionID, generation string) (*SessionJob, error) {
	name, err := sessionJobName(dataDir, sessionID, generation)
	if err != nil {
		return nil, err
	}
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("encode session job name: %w", err)
	}
	handle, _, callErr := openJobObjectW.Call(
		uintptr(jobObjectAssignProcess|jobObjectQuery|jobObjectTerminate|jobObjectSynchronize),
		0,
		uintptr(unsafe.Pointer(namePtr)), // #nosec G103 -- required by OpenJobObjectW.
	)
	if handle == 0 {
		if errors.Is(callErr, windows.ERROR_FILE_NOT_FOUND) {
			return nil, ErrSessionJobNotFound
		}
		return nil, fmt.Errorf("open session job: %w", callErr)
	}
	return &SessionJob{handle: windows.Handle(handle)}, nil
}

// Assign adds one process to the job. Descendants inherit membership.
func (j *SessionJob) Assign(pid int) error {
	if pid <= 0 || uint64(pid) > math.MaxUint32 {
		return fmt.Errorf("assign session job: invalid pid %d", pid)
	}
	pid32 := uint32(pid) // #nosec G115 -- range checked above.
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.handle == 0 {
		return errors.New("assign session job: job is closed")
	}
	processHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		pid32,
	)
	if err != nil {
		return fmt.Errorf("open process %d for session job: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(processHandle) }()
	if err := windows.AssignProcessToJobObject(j.handle, processHandle); err != nil {
		return fmt.Errorf("assign process %d to session job: %w", pid, err)
	}
	return nil
}

// TerminateAndWait kills every current job member and does not return success
// until Windows reports that the job has no active processes. Callers use this
// boundary before reclaiming a session workspace.
func (j *SessionJob) TerminateAndWait(ctx context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.handle == 0 {
		return errors.New("terminate session job: job is closed")
	}
	if err := windows.TerminateJobObject(j.handle, 1); err != nil {
		return fmt.Errorf("terminate session job: %w", err)
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := windows.WaitForSingleObject(j.handle, 0)
		if err != nil {
			return fmt.Errorf("wait for session job: %w", err)
		}
		if result == uint32(windows.WAIT_OBJECT_0) {
			return nil
		}
		if result != uint32(windows.WAIT_TIMEOUT) {
			return fmt.Errorf("wait for session job: unexpected result %d", result)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for session job to empty: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// Close releases this process's handle to the session job.
func (j *SessionJob) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.handle == 0 {
		return nil
	}
	handle := j.handle
	j.handle = 0
	if err := windows.CloseHandle(handle); err != nil {
		return fmt.Errorf("close session job: %w", err)
	}
	return nil
}

func (j *SessionJob) enableKillOnClose() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(
		j.handle,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), // #nosec G103 -- required by QueryInformationJobObject.
		uint32(unsafe.Sizeof(info)),
		nil,
	); err != nil {
		return fmt.Errorf("query session job limits: %w", err)
	}
	info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		j.handle,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), // #nosec G103 -- required by SetInformationJobObject.
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		return fmt.Errorf("set session job limits: %w", err)
	}
	return nil
}

func sessionJobName(dataDir, sessionID, generation string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", errors.New("session job requires a session id")
	}
	generation = strings.TrimSpace(generation)
	if generation == "" {
		return "", errors.New("session job requires a runtime generation")
	}
	scope := strings.TrimSpace(dataDir)
	if scope == "" {
		scope = "default"
	} else {
		absolute, err := filepath.Abs(scope)
		if err != nil {
			return "", fmt.Errorf("resolve session job data dir %q: %w", dataDir, err)
		}
		scope = strings.ToLower(filepath.Clean(absolute))
	}
	digest := sha256.Sum256([]byte(scope + "\x00" + sessionID + "\x00" + generation))
	// Local\ keeps the object inside the interactive logon session. The digest
	// prevents collisions between independent AO_DATA_DIR roots without putting
	// filesystem paths into the Windows object namespace.
	return fmt.Sprintf(`Local\ao-session-%x`, digest[:16]), nil
}

// ResumeSuspendedProcess resumes the initial thread of a process created with
// CREATE_SUSPENDED after its session ownership has been established.
func ResumeSuspendedProcess(pid int) error {
	if pid <= 0 || uint64(pid) > math.MaxUint32 {
		return fmt.Errorf("invalid suspended process pid %d", pid)
	}
	pid32 := uint32(pid) // #nosec G115 -- range checked above.
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()

	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return err
	}
	for {
		if entry.OwnerProcessID == pid32 {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return err
			}
			_, resumeErr := windows.ResumeThread(thread)
			closeErr := windows.CloseHandle(thread)
			return errors.Join(resumeErr, closeErr)
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return fmt.Errorf("primary thread for process %d not found", pid)
			}
			return err
		}
	}
}
