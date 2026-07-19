//go:build linux

package verification

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func verificationSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// linuxVerificationDescendantOwner makes the guardian a child subreaper. If a
// verifier descendant leaves the target process group with setsid(2), killing
// its in-group ancestors reparents it to this guardian instead of init. The
// guardian can then enumerate and terminate only those adopted direct children.
type linuxVerificationDescendantOwner struct{}

func newVerificationDescendantOwner() (*linuxVerificationDescendantOwner, error) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return nil, fmt.Errorf("enable child subreaper: %w", err)
	}
	// Refuse to start a target if procfs cannot provide the direct-child list
	// needed to close the setsid escape after children are adopted.
	if _, err := linuxDirectChildren(); err != nil {
		return nil, fmt.Errorf("read guardian children from procfs: %w", err)
	}
	return &linuxVerificationDescendantOwner{}, nil
}

func (*linuxVerificationDescendantOwner) Close() error { return nil }

func (*linuxVerificationDescendantOwner) Terminate(targetPID int) error {
	// Ensure ordinary in-group descendants are stopped as well. The pidfd
	// cleanup below handles descendants which escaped with setsid.
	killVerificationProcessGroup(targetPID)
	// Give the kernel a scheduling/reparenting turn before observing adopted
	// children; procfs can legitimately lag the group leader's exit.
	time.Sleep(50 * time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := reapExitedLinuxChildren(); err != nil {
			return err
		}
		children, err := linuxDirectChildren()
		if err != nil {
			return err
		}
		if len(children) == 0 {
			// Wait4/ECHILD is the kernel-backed completion barrier. Do not rely
			// on repeated procfs observations, which can race delayed reparenting.
			return nil
		} else {
			for _, pid := range children {
				if err := killLinuxPID(pid); err != nil && !errors.Is(err, syscall.ESRCH) {
					return fmt.Errorf("kill adopted verifier descendant %d: %w", pid, err)
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	children, err := linuxDirectChildren()
	if err != nil {
		return err
	}
	if len(children) != 0 {
		return fmt.Errorf("adopted verifier descendants did not exit: %v", children)
	}
	return nil
}

// killLinuxPID binds the signal to the kernel process identity. Numeric PIDs
// may be reused between a procfs scan and cleanup; pidfds make that race
// harmless (the signal targets the originally opened process or no process).
func killLinuxPID(pid int) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
}

func reapExitedLinuxChildren() error {
	for {
		pid, err := unix.Wait4(-1, nil, unix.WNOHANG, nil)
		if errors.Is(err, unix.ECHILD) || pid == 0 {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reap adopted verifier descendant: %w", err)
		}
	}
}

// Linux exposes children per task, so scan every guardian thread rather than
// assuming the thread-group leader performed every os/exec fork.
func linuxDirectChildren() ([]int, error) {
	paths, err := filepath.Glob("/proc/self/task/*/children")
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, errors.New("no /proc self task children files")
	}
	seen := make(map[int]struct{})
	for _, path := range paths {
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			// Runtime threads can disappear between Glob and ReadFile.
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			return nil, readErr
		}
		for _, field := range strings.Fields(string(body)) {
			pid, convErr := strconv.Atoi(field)
			if convErr != nil || pid <= 0 {
				return nil, fmt.Errorf("invalid procfs child pid %q", field)
			}
			seen[pid] = struct{}{}
		}
	}
	children := make([]int, 0, len(seen))
	for pid := range seen {
		children = append(children, pid)
	}
	return children, nil
}
