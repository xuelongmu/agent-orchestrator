//go:build windows

package verification

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	outputRead, outputWrite, err := os.Pipe()
	if err != nil {
		_ = ownerRead.Close()
		_ = ownerWrite.Close()
		return RunResult{ExitCode: -1}, err
	}
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Dir, spec.Env, ownerRead, outputWrite, outputWrite
	// No verifier code can run before the suspended process is assigned to the
	// kill-on-close Job. This closes the fast-parent/daemonizing-child race.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error { return windows.TerminateJobObject(job, 1) }
	if err = cmd.Start(); err != nil {
		_ = ownerRead.Close()
		_ = ownerWrite.Close()
		_ = outputRead.Close()
		_ = outputWrite.Close()
		return RunResult{ExitCode: -1}, err
	}
	_ = ownerRead.Close()
	_ = outputWrite.Close()
	copyDone := make(chan error, 1)
	output := spec.Output
	if output == nil {
		output = io.Discard
	}
	go func() {
		_, copyErr := io.Copy(output, outputRead)
		copyDone <- copyErr
	}()
	process, openErr := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|0x0800, false, uint32(cmd.Process.Pid)) // PROCESS_SUSPEND_RESUME
	if openErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = outputRead.Close()
		<-copyDone
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
		_ = outputRead.Close()
		<-copyDone
		return RunResult{ExitCode: -1}, err
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	var copyErr error
	copyFinished := false
	guardianDone := false
	for !guardianDone {
		select {
		case err = <-wait:
			guardianDone = true
			_ = ownerWrite.Close()
		case copyErr = <-copyDone:
			copyFinished = true
			if copyErr != nil {
				_ = ownerWrite.Close()
				_ = windows.TerminateJobObject(job, 1)
				err = <-wait
				guardianDone = true
			}
		case <-ctx.Done():
			_ = ownerWrite.Close()
			_ = windows.TerminateJobObject(job, 1)
			err = <-wait
			guardianDone = true
		}
	}
	// A verifier descendant may inherit the guardian's stdout/stderr handles.
	// Terminate the Job as soon as the guardian exits so those handles close;
	// only then drain our explicitly owned copy pipe. Using *os.File on exec.Cmd
	// keeps cmd.Wait independent from os/exec's internal copy goroutines.
	_ = windows.TerminateJobObject(job, 1)
	if !copyFinished {
		copyErr = <-copyDone
	}
	_ = outputRead.Close()
	if ctx.Err() != nil {
		return RunResult{ExitCode: -1}, ctx.Err()
	}
	if copyErr != nil {
		return RunResult{ExitCode: -1}, copyErr
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
	argv, err := prepareWindowsTarget(argv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "prepare verification target: %v\n", err)
		return 126
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Env = targetEnvironment()
	if err := cmd.Start(); err != nil {
		return 126
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-wait:
	case <-ownershipCanceled(owner):
		_ = cmd.Process.Kill()
		waitErr = <-wait
	}
	result, resultErr := processResult(waitErr)
	if resultErr != nil {
		return 126
	}
	return result.ExitCode
}

// prepareWindowsTarget maps npm's batch launcher to node.exe plus npm-cli.js.
// CreateProcess cannot execute .cmd files directly, while invoking cmd.exe
// would violate the verifier's no-shell policy and evaluate user arguments.
func prepareWindowsTarget(argv []string) ([]string, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty verification argv")
	}
	base := strings.ToLower(filepath.Base(argv[0]))
	if base != "npm" && base != "npm.cmd" {
		return append([]string(nil), argv...), nil
	}
	npmCommand := argv[0]
	if !filepath.IsAbs(npmCommand) && filepath.Dir(npmCommand) == "." {
		resolved, err := exec.LookPath("npm.cmd")
		if err != nil {
			return nil, fmt.Errorf("resolve npm.cmd: %w", err)
		}
		npmCommand = resolved
	} else if filepath.Ext(npmCommand) == "" {
		npmCommand += ".cmd"
	}
	npmDir := filepath.Dir(npmCommand)
	nodePath := filepath.Join(npmDir, "node.exe")
	if info, err := os.Stat(nodePath); err != nil || !info.Mode().IsRegular() {
		resolved, lookErr := exec.LookPath("node.exe")
		if lookErr != nil {
			return nil, fmt.Errorf("resolve node.exe: %w", lookErr)
		}
		nodePath = resolved
	}
	npmCLI := filepath.Join(npmDir, "node_modules", "npm", "bin", "npm-cli.js")
	if info, err := os.Stat(npmCLI); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("resolve npm-cli.js beside npm.cmd")
	}
	resolved := make([]string, 0, len(argv)+1)
	resolved = append(resolved, nodePath, npmCLI)
	resolved = append(resolved, argv[1:]...)
	return resolved, nil
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
