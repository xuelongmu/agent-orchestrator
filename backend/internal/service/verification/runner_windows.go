//go:build windows

package verification

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

func runProcessTree(ctx context.Context, spec RunSpec) (RunResult, error) {
	job, err := newKillJob()
	if err != nil {
		return RunResult{ExitCode: -1}, err
	}
	defer func() { _ = windows.CloseHandle(job) }()
	ownerRead, ownerWrite, err := os.Pipe()
	if err != nil {
		return RunResult{ExitCode: -1}, err
	}
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Dir, spec.Env, ownerRead, spec.Output, spec.Output
	// No verifier code can run before the suspended process is assigned to the
	// kill-on-close Job. This closes the fast-parent/daemonizing-child race.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error { return windows.TerminateJobObject(job, 1) }
	if err = cmd.Start(); err != nil {
		_ = ownerRead.Close()
		_ = ownerWrite.Close()
		return RunResult{ExitCode: -1}, err
	}
	_ = ownerRead.Close()
	process, openErr := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|0x0800, false, uint32(cmd.Process.Pid)) // PROCESS_SUSPEND_RESUME
	if openErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return RunResult{ExitCode: -1}, openErr
	}
	defer func() { _ = windows.CloseHandle(process) }()
	if err = windows.AssignProcessToJobObject(job, process); err == nil {
		status, _, _ := ntResumeProcess.Call(uintptr(process))
		if int32(status) < 0 {
			err = fmt.Errorf("resume verification guardian: NTSTATUS %#x", status)
		}
	}
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return RunResult{ExitCode: -1}, err
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err = <-wait:
		_ = ownerWrite.Close()
	case <-ctx.Done():
		_ = ownerWrite.Close()
		_ = windows.TerminateJobObject(job, 1)
		err = <-wait
	}
	if ctx.Err() != nil {
		return RunResult{ExitCode: -1}, ctx.Err()
	}
	return processResult(err)
}

func newKillJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func runHostedProcess(argv []string, owner io.Reader, stdout, stderr io.Writer) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Env = targetEnvironment()
	if err := cmd.Start(); err != nil {
		return 126
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	var err error
	select {
	case err = <-wait:
	case <-ownershipCanceled(owner):
		_ = cmd.Process.Kill()
		err = <-wait
	}
	result, resultErr := processResult(err)
	if resultErr != nil {
		return 126
	}
	return result.ExitCode
}

func processResult(err error) (RunResult, error) {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return RunResult{ExitCode: exit.ExitCode()}, nil
	}
	if err != nil {
		return RunResult{ExitCode: -1}, err
	}
	return RunResult{ExitCode: 0}, nil
}
