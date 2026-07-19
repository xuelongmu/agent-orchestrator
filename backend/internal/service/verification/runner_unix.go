//go:build darwin || linux

package verification

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func runProcessTree(ctx context.Context, spec RunSpec) (RunResult, error) {
	ownerRead, ownerWrite, err := os.Pipe()
	if err != nil {
		return RunResult{ExitCode: -1}, err
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Dir, spec.Env, ownerRead, spec.Output, spec.Output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	// Do not use CommandContext here: its default cancellation can kill the
	// guardian before the owner pipe closes, allowing the target's independent
	// process group to survive. Cancellation is owned by the select below.
	if err := cmd.Start(); err != nil {
		_ = ownerRead.Close()
		_ = ownerWrite.Close()
		return RunResult{ExitCode: -1}, err
	}
	_ = ownerRead.Close()
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err = <-wait:
		_ = ownerWrite.Close()
	case <-ctx.Done():
		_ = ownerWrite.Close()
		// Kill while the leader is still retained by os/exec. Sending a
		// numeric group signal after Wait can target a reused PGID.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-wait
	}
	if ctx.Err() != nil {
		return RunResult{ExitCode: -1}, ctx.Err()
	}
	return processResult(err)
}

func runHostedProcess(argv []string, owner io.Reader, stdout, stderr io.Writer) int {
	descendants, ownerErr := newVerificationDescendantOwner()
	if ownerErr != nil {
		_, _ = io.WriteString(stderr, "prepare verification descendant ownership: "+ownerErr.Error()+"\n")
		return 126
	}
	defer descendants.Close()

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Env = targetEnvironment()
	cmd.SysProcAttr = verificationSysProcAttr()
	if err := cmd.Start(); err != nil {
		return 126
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	var err error
	select {
	case err = <-wait:
	case <-ownershipCanceled(owner):
		killVerificationProcessGroup(cmd.Process.Pid)
		err = <-wait
	}
	if cleanupErr := descendants.Terminate(cmd.Process.Pid); cleanupErr != nil {
		_, _ = io.WriteString(stderr, "terminate verification descendants: "+cleanupErr.Error()+"\n")
		return 126
	}
	result, resultErr := processResult(err)
	if resultErr != nil {
		return 126
	}
	return result.ExitCode
}

func killVerificationProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
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
