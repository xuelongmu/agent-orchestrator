//go:build !darwin && !linux && !windows

package verification

import (
	"context"
	"errors"
	"os/exec"
)

func runProcessTree(ctx context.Context, spec RunSpec) (RunResult, error) {
	if len(spec.Argv) == 0 {
		return RunResult{ExitCode: -1}, errors.New("empty verification argv")
	}
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = spec.Dir, spec.Env, spec.Output, spec.Output
	err := cmd.Run()
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
	return RunResult{}, nil
}
