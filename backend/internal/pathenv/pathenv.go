// Package pathenv centralizes the effective executable search path used by
// daemon-side resolution and spawned sessions.
package pathenv

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Effective returns PATH when the environment supplies one, otherwise the
// platform's system default. getenv is injected so callers that already
// abstract their environment remain straightforward to test.
func Effective(getenv func(string) string) string {
	if path := getenv("PATH"); path != "" {
		return path
	}
	return defaultPATH
}

// AgentBinDir returns AO's configured cross-platform agent wrapper directory.
// It lives beside running.json so AO_RUN_FILE isolation carries wrappers and
// executable lookup together; without an override the canonical location is
// ~/.ao/bin.
func AgentBinDir(getenv func(string) string, userHomeDir func() (string, error)) (string, error) {
	if runFile := strings.TrimSpace(getenv("AO_RUN_FILE")); runFile != "" {
		return filepath.Join(filepath.Dir(runFile), "bin"), nil
	}
	home, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home for AO agent bin: %w", err)
	}
	return filepath.Join(home, ".ao", "bin"), nil
}
