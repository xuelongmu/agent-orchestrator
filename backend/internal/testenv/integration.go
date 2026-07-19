// Package testenv centralizes environment policy for tests that exercise real
// host tools instead of fakes.
package testenv

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
)

const requireIntegrationEnv = "AO_REQUIRE_INTEGRATION"

// RequireExecutable returns executable's resolved path. Missing tools skip the
// integration locally, but fail when CI opts into a trusted integration signal
// with AO_REQUIRE_INTEGRATION=1.
func RequireExecutable(t testing.TB, executable string) string {
	t.Helper()
	path, skip, err := executableRequirement(executable, exec.LookPath, os.Getenv(requireIntegrationEnv) == "1")
	if err != nil {
		t.Fatal(err)
	}
	if skip {
		t.Skipf("%s unavailable", executable)
	}
	return path
}

func executableRequirement(executable string, lookPath func(string) (string, error), required bool) (path string, skip bool, err error) {
	path, lookupErr := lookPath(executable)
	if lookupErr == nil {
		return path, false, nil
	}
	if required {
		return "", false, fmt.Errorf("required integration prerequisite %q is unavailable: %w", executable, lookupErr)
	}
	return "", true, nil
}
