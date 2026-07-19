package verification

import "context"

// OSRunner executes a configured argv directly in a daemon-owned process tree.
type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	return runProcessTree(ctx, spec)
}
