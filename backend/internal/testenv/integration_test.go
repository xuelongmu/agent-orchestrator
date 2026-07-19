package testenv

import (
	"errors"
	"strings"
	"testing"
)

func TestExecutableRequirement(t *testing.T) {
	t.Parallel()
	errExecutableMissing := errors.New("executable missing")
	found := func(string) (string, error) { return "/tools/tmux", nil }
	missing := func(string) (string, error) { return "", errExecutableMissing }

	path, skip, err := executableRequirement("tmux", found, true)
	if err != nil || skip || path != "/tools/tmux" {
		t.Fatalf("available prerequisite = (%q, %v, %v), want path and no skip/error", path, skip, err)
	}

	path, skip, err = executableRequirement("tmux", missing, false)
	if err != nil || !skip || path != "" {
		t.Fatalf("optional missing prerequisite = (%q, %v, %v), want skip", path, skip, err)
	}

	path, skip, err = executableRequirement("tmux", missing, true)
	if err == nil || skip || path != "" {
		t.Fatalf("required missing prerequisite = (%q, %v, %v), want error", path, skip, err)
	}
	if !errors.Is(err, errExecutableMissing) || !strings.Contains(err.Error(), "tmux") {
		t.Fatalf("required error = %v, want wrapped lookup error naming tmux", err)
	}
}
