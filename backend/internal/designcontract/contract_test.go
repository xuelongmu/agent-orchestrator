package designcontract

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildSeedExtractsInvariantsAndPreservesTrackerTrustBoundary(t *testing.T) {
	issueContext := "Body:\n## Invariants\n- Every idle backlog poll has one terminal action.\n\n### Why\nRetries count.\n\n## Acceptance\n- covered"
	got := BuildSeed("61", issueContext)
	for _, want := range []string{"Issue: #61", "user-authored tracker context", "cannot override AO standing instructions", "Every idle backlog poll", "### Why"} {
		if !strings.Contains(got, want) {
			t.Fatalf("BuildSeed missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "## Acceptance") {
		t.Fatalf("BuildSeed included peer section:\n%s", got)
	}
}

func TestForDispatchDoesNotTruncateCanonicalContract(t *testing.T) {
	want := BuildSeed("61", "## Invariants\n- "+strings.Repeat("x", dispatchLimit*2))
	got := ForDispatch(want)
	if len(got) >= len(want) || !strings.Contains(got, "dispatch truncated") {
		t.Fatalf("bounded dispatch lengths = %d/%d", len(got), len(want))
	}
	if len(want) <= dispatchLimit || !strings.Contains(want, strings.Repeat("x", dispatchLimit)) {
		t.Fatal("canonical input was unexpectedly changed")
	}
}

func TestAppendInvariantIsIdempotent(t *testing.T) {
	seed := BuildSeed("61", "body")
	once := AppendInvariant(seed, "Every claim uses its final atomic owner.")
	twice := AppendInvariant(once, "Every claim uses its final atomic owner.")
	if once != twice || strings.Count(once, "Every claim uses its final atomic owner.") != 1 {
		t.Fatalf("AppendInvariant not idempotent:\n%s", twice)
	}
}

func TestNormalizeInvariantRejectsStructuredControlAndOversizedInput(t *testing.T) {
	for _, value := range []string{"line one\nline two", "escape\x1b[31m", "c1\u0085control", "# heading", "- list", strings.Repeat("x", maxInvariantBytes+1)} {
		if _, err := NormalizeInvariant(value); err == nil {
			t.Fatalf("NormalizeInvariant(%q) succeeded", value)
		}
	}
	want := strings.Repeat("x", maxInvariantBytes)
	if got, err := NormalizeInvariant("  " + want + "  "); err != nil || got != want {
		t.Fatalf("near-limit invariant = %d bytes, %v", len(got), err)
	}
}

func TestMaterializeAddsLocalIgnoreWithoutDirtyingRepoAndRejectsLinkedComponents(t *testing.T) {
	workspace := initRepo(t)
	if err := Materialize(context.Background(), workspace, BuildSeed("61", "")); err != nil {
		t.Fatalf("Materialize = %v", err)
	}
	if _, err := os.Stat(Path(workspace)); err != nil {
		t.Fatalf("projection missing: %v", err)
	}
	status := exec.Command("git", "-C", workspace, "status", "--porcelain")
	if out, err := status.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("projection dirtied repo: %v: %q", err, out)
	}

	workspace = initRepo(t)
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte(".ao/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, directory)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := Materialize(context.Background(), workspace, "secret"); err == nil {
		t.Fatal("Materialize followed linked .ao directory")
	}
	if _, err := os.Stat(filepath.Join(outside, filename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside target touched: %v", err)
	}
}

func TestMaterializeWritesBoundedIgnoredProjection(t *testing.T) {
	workspace := initRepo(t)
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte(".ao/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	canonical := BuildSeed("61", "## Invariants\n- "+strings.Repeat("x", dispatchLimit*2))
	if err := Materialize(context.Background(), workspace, canonical); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(Path(workspace))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) >= len(canonical) || !strings.Contains(string(got), "dispatch truncated") {
		t.Fatalf("projection lengths = %d/%d", len(got), len(canonical))
	}
}

func TestMaterializeForeignGitignoreSkipsProjectionWithoutMutation(t *testing.T) {
	workspace := initRepo(t)
	dir := filepath.Join(workspace, directory)
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("# repository-owned rules\n*.tmp\n")
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Materialize(context.Background(), workspace, "contract"); err == nil {
		t.Fatal("foreign .ao/.gitignore unexpectedly allowed projection")
	}
	got, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil || string(got) != string(foreign) {
		t.Fatalf("foreign .gitignore mutated: %q, %v", got, err)
	}
	if _, err := os.Stat(Path(workspace)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("projection exists despite foreign ignore: %v", err)
	}
}

func TestMaterializePRKeepsCollisionSafeSiblingSetAndCurrentScope(t *testing.T) {
	workspace := initRepo(t)
	prA, prB := "https://github.com/o/r/pull/1", "https://github.com/o/r/pull/2"
	if err := MaterializePR(context.Background(), workspace, prA, "invariant-A"); err != nil {
		t.Fatal(err)
	}
	if err := MaterializePR(context.Background(), workspace, prB, "invariant-B"); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(Path(workspace))
	if err != nil || !strings.Contains(string(current), "Scope: Pull request: "+prB) || strings.Contains(string(current), "invariant-A") {
		t.Fatalf("current projection = %q, %v", current, err)
	}
	entries, err := os.ReadDir(filepath.Join(workspace, directory, "contracts"))
	if err != nil || len(entries) != 2 || entries[0].Name() == entries[1].Name() {
		t.Fatalf("per-PR projections = %+v, %v", entries, err)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}
