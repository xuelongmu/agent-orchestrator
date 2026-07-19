//go:build darwin || linux

package verification

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

func runProcessTree(ctx context.Context, spec RunSpec) (RunResult, error) {
	if len(spec.Argv) == 0 {
		return RunResult{ExitCode: -1}, errors.New("empty verification argv")
	}
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = spec.Dir, spec.Env, spec.Output, spec.Output
	cmd.SysProcAttr = verificationSysProcAttr()
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
	if err := cmd.Start(); err != nil {
		return RunResult{ExitCode: -1}, err
	}
	err := cmd.Wait()
	// A verifier that exits after daemonizing a child must not leak that child.
	// The process-group id remains the original leader pid after it exits.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if ctx.Err() != nil {
		return RunResult{ExitCode: -1}, ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return RunResult{ExitCode: exit.ExitCode()}, nil
	}
	if err != nil {
		return RunResult{ExitCode: -1}, err
	}
	return RunResult{ExitCode: 0}, nil
}
