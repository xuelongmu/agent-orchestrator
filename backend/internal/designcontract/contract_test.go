package designcontract

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestForDispatchRetainsLearnedTailWithoutChangingCanonicalContract(t *testing.T) {
	want := BuildSeed("61", "## Invariants\n- "+strings.Repeat("x", dispatchLimit*2))
	want = AppendInvariant(want, "Every learned tail invariant remains visible.")
	got := ForDispatch(want)
	if len(got) >= len(want) || !strings.Contains(got, "middle omitted") || !strings.Contains(got, "Every learned tail invariant remains visible.") {
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

func TestAppendInvariantDeduplicatesOnlyNormalizedExactLines(t *testing.T) {
	seed := BuildSeed("61", "## Invariants\n- Every claim uses its final atomic owner.")
	partial := AppendInvariant(seed, "final atomic owner.")
	differentCase := AppendInvariant(partial, "Every claim uses its FINAL atomic owner.")
	exact := AppendInvariant(differentCase, "  Every claim uses its final atomic owner.  ")
	if exact != differentCase {
		t.Fatalf("exact invariant appended twice:\n%s", exact)
	}
	for _, want := range []string{"- final atomic owner.", "- Every claim uses its FINAL atomic owner."} {
		if !strings.Contains(exact, want) {
			t.Fatalf("missing distinct exact line %q:\n%s", want, exact)
		}
	}
	if HasInvariant(exact, "atomic owner") || !HasInvariant(exact, "Every claim uses its final atomic owner.") {
		t.Fatalf("exact-line lookup used substring semantics:\n%s", exact)
	}
}

func TestNormalizeInvariantRejectsStructuredControlAndOversizedInput(t *testing.T) {
	for _, value := range []string{"line one\nline two", "escape\x1b[31m", "c1\u0085control", "trimmed-c1\u0085", "# heading", "- list", "* list", "+ list", "1. list", "2) list", "<tag>", strings.Repeat("x", maxInvariantBytes+1)} {
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

func TestWriteProjectionRejectsDirectoryReplacedByLinkAfterRootOpen(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, directory), 0o750); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := os.Rename(filepath.Join(workspace, directory), filepath.Join(workspace, ".ao-real")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	linkTestDirectory(t, outside, filepath.Join(workspace, directory))
	if err := writeProjection(root, directory, filepath.ToSlash(filepath.Join(directory, filename)), "must stay confined"); err == nil {
		t.Fatal("handle-relative write followed replacement link")
	}
	if _, err := os.Stat(filepath.Join(outside, filename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement link target touched: %v", err)
	}
}

func TestMaterializeRejectsLinkedGitignoreAndContractTargets(t *testing.T) {
	for _, targetName := range []string{".gitignore", filename} {
		t.Run(targetName, func(t *testing.T) {
			workspace := initRepo(t)
			dir := filepath.Join(workspace, directory)
			if err := os.Mkdir(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside.txt")
			before := "outside must not change\n"
			if err := os.WriteFile(outside, []byte(before), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dir, targetName)); err != nil {
				t.Skipf("file symlinks unavailable: %v", err)
			}
			if err := Materialize(context.Background(), workspace, "contract"); err == nil {
				t.Fatalf("linked %s unexpectedly allowed projection", targetName)
			}
			got, err := os.ReadFile(outside)
			if err != nil || string(got) != before {
				t.Fatalf("outside target changed: %q, %v", got, err)
			}
		})
	}
}

func TestMaterializeWritesFullIgnoredProjection(t *testing.T) {
	workspace := initRepo(t)
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte(".ao/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	canonical := BuildSeed("61", "## Invariants\n- "+strings.Repeat("x", dispatchLimit*2))
	canonical = AppendInvariant(canonical, "Every learned tail invariant remains visible.")
	if err := Materialize(context.Background(), workspace, canonical); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(Path(workspace))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), canonical) || !strings.Contains(string(got), "Every learned tail invariant remains visible.") || strings.Contains(string(got), "middle omitted") {
		t.Fatalf("projection did not retain full canonical bytes: projection=%d canonical=%d", len(got), len(canonical))
	}
}

func TestMaterializeSanitizesProjectionWithoutChangingCanonicalBytes(t *testing.T) {
	workspace := initRepo(t)
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte(".ao/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	canonical := BuildSeed("61", "## Invariants\n- Untrusted \x1b[31mred\u0085next\x00 invariant")
	prURL := "https://example.test/pull/1\x1b[2J"
	if err := MaterializePR(context.Background(), workspace, prURL, canonical); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(Path(workspace))
	if err != nil {
		t.Fatal(err)
	}
	projection := string(got)
	for _, forbidden := range []string{"\x1b", "\x00", "\u0085"} {
		if strings.Contains(projection, forbidden) {
			t.Fatalf("projection retained control %q: %q", forbidden, projection)
		}
	}
	for _, want := range []string{"[31mred", "next", "[2J"} {
		if !strings.Contains(projection, want) {
			t.Fatalf("projection lost sanitized text %q: %q", want, projection)
		}
	}
	if !strings.Contains(canonical, "\x1b") || !strings.Contains(canonical, "\u0085") {
		t.Fatal("projection sanitization mutated canonical input")
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

func TestMaterializeTrackedAODirectorySkipsProjection(t *testing.T) {
	workspace := initRepo(t)
	dir := filepath.Join(workspace, directory)
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(dir, "owned.txt")
	if err := os.WriteFile(owned, []byte("repository-owned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", workspace, "add", ".ao/owned.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if err := Materialize(context.Background(), workspace, "contract"); err == nil {
		t.Fatal("tracked .ao unexpectedly allowed projection")
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tracked .ao was mutated: %v", err)
	}
	got, err := os.ReadFile(owned)
	if err != nil || string(got) != "repository-owned\n" {
		t.Fatalf("tracked content changed: %q, %v", got, err)
	}
}

func TestMaterializeLinkedWorktreeDoesNotMutateSharedGitMetadata(t *testing.T) {
	main := initRepo(t)
	for _, args := range [][]string{{"config", "user.email", "ao@example.com"}, {"config", "user.name", "AO Tests"}} {
		if out, err := exec.Command("git", append([]string{"-C", main}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(main, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "initial"}} {
		if out, err := exec.Command("git", append([]string{"-C", main}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	linked := filepath.Join(t.TempDir(), "linked")
	if out, err := exec.Command("git", "-C", main, "worktree", "add", "-q", "-b", "projection-test", linked).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	common, err := exec.Command("git", "-C", linked, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		t.Fatal(err)
	}
	commonDir := strings.TrimSpace(string(common))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(linked, commonDir)
	}
	exclude := filepath.Join(commonDir, "info", "exclude")
	before, _ := os.ReadFile(exclude)
	if err := Materialize(context.Background(), linked, "contract"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(exclude)
	if string(after) != string(before) {
		t.Fatalf("shared git exclude mutated:\nbefore=%q\nafter=%q", before, after)
	}
	if out, err := exec.Command("git", "-C", linked, "status", "--porcelain").CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("linked projection dirtied worktree: %v: %q", err, out)
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

func linkTestDirectory(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	} else if runtime.GOOS != "windows" {
		t.Skipf("creating symlink: %v", err)
	} else {
		cmd := exec.Command("cmd", "/c", "mklink", "/J", link, target)
		if out, junctionErr := cmd.CombinedOutput(); junctionErr != nil {
			t.Skipf("creating symlink or junction: symlink: %v; junction: %v: %s", err, junctionErr, out)
		}
	}
}
