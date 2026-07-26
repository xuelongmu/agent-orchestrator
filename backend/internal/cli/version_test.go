package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestVersionJSONReportsProvenanceAndCapabilities(t *testing.T) {
	oldVersion, oldCommit, oldDate := Version, Commit, Date
	oldRepository, oldChannel := Repository, Channel
	t.Cleanup(func() {
		Version, Commit, Date = oldVersion, oldCommit, oldDate
		Repository, Channel = oldRepository, oldChannel
	})
	Version = "1.2.3-nightly.202607260100"
	Commit = "abcdef123456"
	Date = "2026-07-26T01:00:00Z"
	Repository = "example/agent-orchestrator"
	Channel = "nightly"

	out, errOut, err := executeCLI(t, Deps{}, "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v\nstderr=%s", err, errOut)
	}
	var got BuildInfo
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode version JSON: %v\n%s", err, out)
	}
	if got.Version != Version || got.Commit != Commit || got.BuiltAt != Date || got.Repository != Repository || got.Channel != Channel {
		t.Fatalf("build info = %#v", got)
	}
	for _, capability := range []string{"codex-primary-worktree-hooks-v1", "project-environment-patch-v1"} {
		if !slices.Contains(got.Capabilities, capability) {
			t.Errorf("capabilities %v missing %q", got.Capabilities, capability)
		}
	}
}

func TestVersionTextRemainsCompatible(t *testing.T) {
	oldVersion, oldCommit, oldDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = oldVersion, oldCommit, oldDate })
	Version, Commit, Date = "1.2.3", "abc", "2026-07-26"

	out, errOut, err := executeCLI(t, Deps{}, "version")
	if err != nil {
		t.Fatalf("version: %v\nstderr=%s", err, errOut)
	}
	if strings.TrimSpace(out) != "1.2.3 commit abc built 2026-07-26" {
		t.Fatalf("version output = %q", out)
	}
}
