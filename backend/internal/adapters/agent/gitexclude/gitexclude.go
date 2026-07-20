// Package gitexclude serializes additive updates to Git's repository-local
// info/exclude file across agent adapters and linked worktrees.
package gitexclude

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/hookutil"
)

// EnsurePattern adds pattern to excludePath exactly once while preserving all
// existing rules. All callers coordinate through the adjacent .ao.lock file.
// onContention is an optional observation hook used by concurrency tests.
func EnsurePattern(excludePath, pattern, comment string, onContention func()) error {
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(excludePath), err)
	}
	unlock, err := Acquire(excludePath+".ao.lock", onContention)
	if err != nil {
		return fmt.Errorf("lock %s: %w", excludePath, err)
	}
	defer unlock()

	data, err := os.ReadFile(excludePath) //nolint:gosec // path resolved from repository metadata
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", excludePath, err)
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || containsPattern(data, pattern) {
		return nil
	}
	body := strings.TrimRight(string(data), "\r\n")
	if body != "" {
		body += "\n"
	}
	comment = strings.TrimSpace(comment)
	if comment != "" {
		body += comment + "\n"
	}
	body += pattern + "\n"
	if err := hookutil.AtomicWriteFile(excludePath, []byte(body), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", excludePath, err)
	}
	return nil
}

func containsPattern(data []byte, pattern string) bool {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(strings.TrimSuffix(line, "\r")) == pattern {
			return true
		}
	}
	return false
}
