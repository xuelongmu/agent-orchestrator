//go:build !darwin && !linux && !windows

package verification

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
)

func runProcessTree(ctx context.Context, spec RunSpec) (RunResult, error) {
	if len(spec.Argv) == 0 {
		return RunResult{ExitCode: -1}, errors.New("empty verification argv")
	}
	ownerRead, ownerWrite, err := os.Pipe()
	if err != nil {
		return RunResult{ExitCode: -1}, err
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Dir, spec.Env, ownerRead, spec.Output, spec.Output
	if err = cmd.Start(); err != nil {
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
		err = <-wait
	}
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

func runHostedProcess(argv []string, owner io.Reader, stdout, stderr io.Writer) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = targetEnvironment()
	cmd.Stdout, cmd.Stderr = stdout, stderr
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
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if err != nil {
		return 126
	}
	return 0
}
