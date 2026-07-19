package verification

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

const verifyHostEnv = "AO_INTERNAL_VERIFY_HOST"

// OSRunner executes a configured argv through a daemon-owned guardian process.
// HostArgv is test-only; production uses the current ao executable.
type OSRunner struct{ HostArgv []string }

// Run owns the guardian until the command exits or the request is canceled.
func (r OSRunner) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	if len(spec.Argv) == 0 {
		return RunResult{ExitCode: -1}, errors.New("empty verification argv")
	}
	payload, err := json.Marshal(spec.Argv)
	if err != nil {
		return RunResult{ExitCode: -1}, err
	}
	host := r.HostArgv
	if len(host) == 0 {
		exe, exeErr := os.Executable()
		if exeErr != nil {
			return RunResult{ExitCode: -1}, exeErr
		}
		host = []string{exe}
	}
	hostSpec := spec
	hostSpec.Argv = append([]string(nil), host...)
	hostSpec.Env = append(append([]string(nil), spec.Env...), verifyHostEnv+"="+base64.RawURLEncoding.EncodeToString(payload))
	return runProcessTree(ctx, hostSpec)
}

// RunHostFromEnvironment runs the internal verification guardian before CLI
// parsing. It returns handled=true only for a daemon-created guardian process.
func RunHostFromEnvironment() (code int, handled bool) {
	encoded := os.Getenv(verifyHostEnv)
	if encoded == "" {
		return 0, false
	}
	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return 126, true
	}
	var argv []string
	if err := json.Unmarshal(body, &argv); err != nil || len(argv) == 0 {
		return 126, true
	}
	return runHostedProcess(argv, os.Stdin, os.Stdout, os.Stderr), true
}

func ownershipCanceled(owner io.Reader) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, owner)
		close(done)
	}()
	return done
}

// targetEnvironment removes the daemon-to-guardian marker before the
// guardian starts the configured verification target. Leaving the marker in
// the environment would cause an ao executable used as a target to interpret
// itself recursively as another guardian.
func targetEnvironment() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(name, verifyHostEnv) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}
