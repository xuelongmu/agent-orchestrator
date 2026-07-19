package designcontract

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestParseReviewFixInvariantDeclarationRequiresExactFinalTrailer(t *testing.T) {
	valid := `fix: preserve ownership

The parser token in prose is not enough.

AO-Review-Fix-Invariant: {"pr":"https://github.com/o/r/pull/7","mode":"preserve","invariant":"Every write has one owner."}`
	got, err := ParseReviewFixInvariantDeclaration(valid)
	if err != nil {
		t.Fatalf("parse valid trailer: %v", err)
	}
	if got.PR != "https://github.com/o/r/pull/7" || got.Mode != "preserve" || got.Invariant != "Every write has one owner." {
		t.Fatalf("declaration = %+v", got)
	}

	tests := []struct {
		name    string
		message string
		want    error
	}{
		{"missing", "fix: no declaration", ErrReviewFixDeclarationMissing},
		{"body only", "AO-Review-Fix-Invariant: {}\n\nmore prose", ErrReviewFixDeclarationMalformed},
		{"duplicate", valid + "\nAO-Review-Fix-Invariant: {}", ErrReviewFixDeclarationMalformed},
		{"unknown field", "x\n\nAO-Review-Fix-Invariant: {\"pr\":\"p\",\"mode\":\"add\",\"invariant\":\"i\",\"other\":true}", ErrReviewFixDeclarationMalformed},
		{"duplicate key", "x\n\nAO-Review-Fix-Invariant: {\"pr\":\"sibling\",\"pr\":\"p\",\"mode\":\"add\",\"invariant\":\"i\"}", ErrReviewFixDeclarationMalformed},
		{"trailing json", "x\n\nAO-Review-Fix-Invariant: {\"pr\":\"p\",\"mode\":\"add\",\"invariant\":\"i\"} {}", ErrReviewFixDeclarationMalformed},
		{"bad mode", "x\n\nAO-Review-Fix-Invariant: {\"pr\":\"p\",\"mode\":\"new\",\"invariant\":\"i\"}", ErrReviewFixDeclarationMalformed},
		{"multiline value", "x\n\nAO-Review-Fix-Invariant: {\"pr\":\"p\",\"mode\":\"add\",\"invariant\":\"a\\nb\"}", ErrReviewFixDeclarationMalformed},
		{"leading whitespace", "x\n\n AO-Review-Fix-Invariant: {\"pr\":\"p\",\"mode\":\"add\",\"invariant\":\"i\"}", ErrReviewFixDeclarationMissing},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseReviewFixInvariantDeclaration(tc.message)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestHasExactInvariantDoesNotNormalizeDeclaration(t *testing.T) {
	contract := "## Invariants\n\n- Preserve exact casing.\n"
	if !HasExactInvariant(contract, "Preserve exact casing.") {
		t.Fatal("exact invariant not found")
	}
	for _, near := range []string{"preserve exact casing.", " Preserve exact casing.", "Preserve exact casing. "} {
		if HasExactInvariant(contract, near) {
			t.Fatalf("near match %q unexpectedly accepted", near)
		}
	}
}

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
	target := filepath.ToSlash(filepath.Join(directory, filename))
	if err := writeProjection(root, target, target, projectionContent(target, "scope", "must stay confined")); err == nil {
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

func TestMaterializeRejectsInRootLinkedProjectionTargets(t *testing.T) {
	t.Run("gitignore symlink", func(t *testing.T) {
		workspace := initRepo(t)
		dir := filepath.Join(workspace, directory)
		if err := os.Mkdir(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		source := filepath.Join(workspace, "crafted-gitignore")
		before := []byte(projectionGitignoreContent())
		if err := os.WriteFile(source, before, 0o600); err != nil {
			t.Fatal(err)
		}
		linkTestFile(t, source, filepath.Join(dir, ".gitignore"))
		if err := Materialize(context.Background(), workspace, "new canonical"); err == nil {
			t.Fatal("in-root gitignore symlink unexpectedly initialized projection ownership")
		}
		got, err := os.ReadFile(source)
		if err != nil || string(got) != string(before) {
			t.Fatalf("crafted gitignore source changed: %q, %v", got, err)
		}
		if _, err := os.Stat(Path(workspace)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("projection written through linked gitignore: %v", err)
		}
	})

	t.Run("file symlink", func(t *testing.T) {
		workspace := initRepo(t)
		dir := filepath.Join(workspace, directory)
		if err := os.Mkdir(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(projectionGitignoreContent()), 0o600); err != nil {
			t.Fatal(err)
		}
		targetRel := filepath.ToSlash(filepath.Join(directory, filename))
		source := filepath.Join(dir, "crafted.md")
		before := []byte(projectionContent(targetRel, "crafted", "foreign bytes must survive"))
		if err := os.WriteFile(source, before, 0o600); err != nil {
			t.Fatal(err)
		}
		linkTestFile(t, source, Path(workspace))
		if err := Materialize(context.Background(), workspace, "new canonical"); err == nil {
			t.Fatal("in-root file symlink unexpectedly allowed projection")
		}
		got, err := os.ReadFile(source)
		if err != nil || string(got) != string(before) {
			t.Fatalf("in-root symlink source changed: %q, %v", got, err)
		}
	})

	t.Run("contracts directory junction", func(t *testing.T) {
		workspace := initRepo(t)
		dir := filepath.Join(workspace, directory)
		if err := os.Mkdir(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(projectionGitignoreContent()), 0o600); err != nil {
			t.Fatal(err)
		}
		prURL := "https://github.com/o/r/pull/31"
		targetRel := testKeyedProjectionRelative(prURL)
		sourceDir := filepath.Join(workspace, "crafted-contracts")
		if err := os.Mkdir(sourceDir, 0o750); err != nil {
			t.Fatal(err)
		}
		source := filepath.Join(sourceDir, filepath.Base(targetRel))
		before := []byte(projectionContent(targetRel, "crafted", "junction bytes must survive"))
		if err := os.WriteFile(source, before, 0o600); err != nil {
			t.Fatal(err)
		}
		linkTestDirectory(t, sourceDir, filepath.Join(dir, "contracts"))
		if err := MaterializePR(context.Background(), workspace, prURL, "new canonical"); err == nil {
			t.Fatal("in-root contracts junction unexpectedly allowed projection")
		}
		got, err := os.ReadFile(source)
		if err != nil || string(got) != string(before) {
			t.Fatalf("junction source changed: %q, %v", got, err)
		}
	})
}

func TestOpenVerifiedSubrootRejectsInRootParentReplacementWithoutMutation(t *testing.T) {
	workspace := initRepo(t)
	dir := filepath.Join(workspace, directory)
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	targetRel := filepath.ToSlash(filepath.Join(directory, filename))
	target := filepath.Join(dir, filename)
	before := []byte(projectionContent(targetRel, "original", "must remain unchanged"))
	if err := os.WriteFile(target, before, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	expected, err := root.Lstat(directory)
	if err != nil {
		t.Fatal(err)
	}
	realPath := filepath.Join(workspace, ".ao-real")
	if err := os.Rename(dir, realPath); err != nil {
		t.Fatal(err)
	}
	linkTestDirectoryInRoot(t, workspace, ".ao-real", dir)
	if child, _, err := openVerifiedSubroot(root, directory, expected); err == nil {
		_ = child.Close()
		t.Fatal("in-root parent replacement unexpectedly passed identity validation")
	}
	got, err := os.ReadFile(filepath.Join(realPath, filename))
	if err != nil || string(got) != string(before) {
		t.Fatalf("renamed .ao-real projection changed: %q, %v", got, err)
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

func TestMaterializeDoesNotClaimForeignGitignoreStagingPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{
			name: "gitfile",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("gitdir: ../foreign\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "empty regular file",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "gitignore prefix regular file",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				content := projectionGitignoreContent()
				if err := os.WriteFile(path, []byte(content[:len(content)/2]), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "empty directory",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path, 0o750); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "directory",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path, 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "foreign"), []byte("must survive\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := initRepo(t)
			dir := filepath.Join(workspace, directory)
			if err := os.Mkdir(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			stage := filepath.Join(dir, ".git")
			tc.prepare(t, stage)
			foreignPath := filepath.Join(stage, "foreign")
			if tc.name != "directory" && tc.name != "empty directory" {
				foreignPath = stage
			}
			before, err := os.ReadFile(foreignPath)
			if tc.name == "empty directory" {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("empty foreign directory was not empty: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if err := Materialize(context.Background(), workspace, "canonical"); err == nil {
				t.Fatal("foreign .ao/.git unexpectedly claimed as AO staging")
			}
			after, err := os.ReadFile(foreignPath)
			if tc.name == "empty directory" {
				entries, readErr := os.ReadDir(stage)
				if readErr != nil || len(entries) != 0 {
					t.Fatalf("empty foreign final .ao/.git was mutated: %+v, %v", entries, readErr)
				}
			} else if err != nil || string(after) != string(before) {
				t.Fatalf("foreign staging state changed: %q, %v", after, err)
			}
		})
	}
}

func TestGitignoreBootstrapCrashRecoveryMatrix(t *testing.T) {
	const bootstrapName = ".bootstrap-0123456789abcdef0123456789abcdef.stage"
	for _, tc := range []struct {
		name        string
		prepare     func(*testing.T, string)
		innerExists bool
	}{
		{
			name: "crash after outer container mkdir",
			prepare: func(t *testing.T, aoDir string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(aoDir, bootstrapName), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "crash after nested exact git mkdir",
			prepare: func(t *testing.T, aoDir string) {
				t.Helper()
				stage := filepath.Join(aoDir, bootstrapName, gitignoreStageDirectory)
				if err := os.MkdirAll(stage, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			innerExists: true,
		},
		{
			name: "crash after zero-byte marker create",
			prepare: func(t *testing.T, aoDir string) {
				t.Helper()
				stage := filepath.Join(aoDir, bootstrapName, gitignoreStageDirectory)
				if err := os.MkdirAll(stage, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(stage, gitignoreStageMarker), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			innerExists: true,
		},
		{
			name: "crash after authenticated marker sync",
			prepare: func(t *testing.T, aoDir string) {
				t.Helper()
				stage := filepath.Join(aoDir, bootstrapName, gitignoreStageDirectory)
				if err := os.MkdirAll(stage, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(stage, gitignoreStageMarker), gitignoreStageMarkerContent(bootstrapName), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			innerExists: true,
		},
		{
			name: "crash after no-replace rename before payload creation",
			prepare: func(t *testing.T, aoDir string) {
				t.Helper()
				stage := filepath.Join(aoDir, bootstrapName, gitignoreStageDirectory)
				if err := os.MkdirAll(stage, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(stage, gitignoreStageMarker), gitignoreStageMarkerContent(bootstrapName), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(stage, filepath.Join(aoDir, gitignoreStageDirectory)); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := initRepo(t)
			aoDir := filepath.Join(workspace, directory)
			if err := os.Mkdir(aoDir, 0o750); err != nil {
				t.Fatal(err)
			}
			tc.prepare(t, aoDir)
			// Crash/error state must be invisible before any restart or AO-owned
			// .ao/.gitignore exists.
			assertCleanGitStatus(t, workspace)
			if err := Materialize(context.Background(), workspace, "recovered complete contract"); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(Path(workspace))
			if err != nil || !strings.Contains(string(got), "recovered complete contract") {
				t.Fatalf("recovered projection = %q, %v", got, err)
			}
			bootstrapPath := filepath.Join(aoDir, bootstrapName)
			if _, stageErr := os.Stat(bootstrapPath); stageErr != nil {
				t.Fatalf("abandoned bootstrap container was enumerated or mutated: %v", stageErr)
			}
			_, innerErr := os.Stat(filepath.Join(bootstrapPath, gitignoreStageDirectory))
			if tc.innerExists && innerErr != nil {
				t.Fatalf("abandoned nested stage was enumerated or consumed: %v", innerErr)
			}
			if !tc.innerExists && !errors.Is(innerErr, os.ErrNotExist) {
				t.Fatalf("unexpected abandoned nested stage state: %v", innerErr)
			}
			assertCleanGitStatus(t, workspace)
		})
	}
}

func TestMaterializeUpgradesLegacyZeroByteFinalStageMarker(t *testing.T) {
	workspace := initRepo(t)
	aoDir := filepath.Join(workspace, directory)
	if err := os.MkdirAll(filepath.Join(aoDir, gitignoreStageDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aoDir, ".gitignore"), []byte(projectionGitignoreContent()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aoDir, gitignoreStageDirectory, gitignoreStageMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	targetRel := filepath.ToSlash(filepath.Join(directory, filename))
	legacyProjection := projectionContent(targetRel, "Session draft (no PR identity yet)", "legacy complete contract")
	if err := os.WriteFile(Path(workspace), []byte(legacyProjection), 0o600); err != nil {
		t.Fatal(err)
	}
	assertCleanGitStatus(t, workspace)

	if err := Materialize(context.Background(), workspace, "upgraded complete contract"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(Path(workspace))
	if err != nil || !strings.Contains(string(got), "upgraded complete contract") || strings.Contains(string(got), "legacy complete contract") {
		t.Fatalf("upgraded projection = %q, %v", got, err)
	}
	marker, err := os.ReadFile(filepath.Join(aoDir, gitignoreStageDirectory, gitignoreStageMarker))
	if err != nil || len(marker) != 0 {
		t.Fatalf("legacy final marker was not accepted in place: %q, %v", marker, err)
	}
	assertCleanGitStatus(t, workspace)
}

func TestLegacyZeroByteFinalMarkerDoesNotOverrideForeignProjection(t *testing.T) {
	workspace := initRepo(t)
	aoDir := filepath.Join(workspace, directory)
	if err := os.MkdirAll(filepath.Join(aoDir, gitignoreStageDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aoDir, ".gitignore"), []byte(projectionGitignoreContent()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aoDir, gitignoreStageDirectory, gitignoreStageMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("foreign projection must survive\n")
	if err := os.WriteFile(Path(workspace), foreign, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Materialize(context.Background(), workspace, "replacement"); err == nil {
		t.Fatal("legacy marker bypassed AO-owned projection validation")
	}
	got, err := os.ReadFile(Path(workspace))
	if err != nil || string(got) != string(foreign) {
		t.Fatalf("foreign projection changed: %q, %v", got, err)
	}
}

func TestGitignoreBootstrapRenameErrorIsImmediatelyInvisibleAndRecoverable(t *testing.T) {
	workspace := initRepo(t)
	original := publishGitignoreStageDirectory
	publishGitignoreStageDirectory = func(_, _ *os.Root, _, _ string, _ os.FileInfo) error {
		return errors.New("injected no-replace directory rename")
	}
	if err := Materialize(context.Background(), workspace, "complete after restart"); err == nil {
		t.Fatal("injected bootstrap rename error was not reported")
	}
	// No AO-owned ignore file exists yet, so cleanliness here proves the nested
	// exact .git component hid the abandoned stage immediately.
	if _, err := os.Stat(filepath.Join(workspace, directory, ".gitignore")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gitignore appeared after failed bootstrap rename: %v", err)
	}
	assertCleanGitStatus(t, workspace)

	publishGitignoreStageDirectory = original
	t.Cleanup(func() { publishGitignoreStageDirectory = original })
	if err := Materialize(context.Background(), workspace, "complete after restart"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(Path(workspace))
	if err != nil || !strings.Contains(string(got), "complete after restart") {
		t.Fatalf("recovered projection = %q, %v", got, err)
	}
	assertCleanGitStatus(t, workspace)
}

func TestMaterializeDoesNotInitializeOwnershipOverForeignTargets(t *testing.T) {
	prURL := "https://github.com/o/r/pull/7"
	for _, tc := range []struct {
		name        string
		target      func(string) string
		materialize func(string) error
	}{
		{
			name:   "current projection",
			target: Path,
			materialize: func(workspace string) error {
				return Materialize(context.Background(), workspace, "canonical")
			},
		},
		{
			name: "keyed projection",
			target: func(workspace string) string {
				return testKeyedProjectionPath(workspace, prURL)
			},
			materialize: func(workspace string) error {
				return MaterializePR(context.Background(), workspace, prURL, "canonical")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := initRepo(t)
			target := tc.target(workspace)
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				t.Fatal(err)
			}
			foreign := []byte("foreign contract bytes\n")
			if err := os.WriteFile(target, foreign, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := tc.materialize(workspace); err == nil {
				t.Fatal("foreign target unexpectedly allowed AO projection ownership")
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != string(foreign) {
				t.Fatalf("foreign target changed: %q, %v", got, err)
			}
			if _, err := os.Stat(filepath.Join(workspace, directory, ".gitignore")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("projection ownership initialized despite foreign target: %v", err)
			}
		})
	}
}

func TestMaterializeRefreshesOnlyGenuineAOProjectionsAcrossRestart(t *testing.T) {
	workspace := initRepo(t)
	prURL := "https://github.com/o/r/pull/8"
	if err := MaterializePR(context.Background(), workspace, prURL, "first canonical"); err != nil {
		t.Fatal(err)
	}
	currentPath := Path(workspace)
	keyedPath := testKeyedProjectionPath(workspace, prURL)
	for _, target := range []string{currentPath, keyedPath} {
		got, err := os.ReadFile(target)
		if err != nil || !strings.Contains(string(got), projectionOwnershipVersion) {
			t.Fatalf("initial AO projection %s lacks ownership contract: %q, %v", target, got, err)
		}
	}

	// A second call has no in-memory ownership state, matching refresh after a
	// daemon restart. Deterministic ownership markers must allow the refresh.
	refreshErr := MaterializePR(context.Background(), workspace, prURL, "second canonical")
	if refreshErr != nil {
		t.Fatal(refreshErr)
	}
	for _, target := range []string{currentPath, keyedPath} {
		got, err := os.ReadFile(target)
		if err != nil || !strings.Contains(string(got), "second canonical") || strings.Contains(string(got), "first canonical") {
			t.Fatalf("AO projection %s was not refreshed: %q, %v", target, got, err)
		}
	}

	for _, foreignTarget := range []string{currentPath, keyedPath} {
		t.Run(filepath.Base(foreignTarget), func(t *testing.T) {
			beforeCurrent, err := os.ReadFile(currentPath)
			if err != nil {
				t.Fatal(err)
			}
			beforeKeyed, err := os.ReadFile(keyedPath)
			if err != nil {
				t.Fatal(err)
			}
			foreign := []byte("foreign replacement must survive\n")
			if err := os.WriteFile(foreignTarget, foreign, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := MaterializePR(context.Background(), workspace, prURL, "third canonical"); err == nil {
				t.Fatal("foreign replacement unexpectedly accepted as AO-owned")
			}
			got, err := os.ReadFile(foreignTarget)
			if err != nil || string(got) != string(foreign) {
				t.Fatalf("foreign replacement changed: %q, %v", got, err)
			}
			other := currentPath
			wantOther := beforeCurrent
			if foreignTarget == currentPath {
				other, wantOther = keyedPath, beforeKeyed
			}
			got, err = os.ReadFile(other)
			if err != nil || string(got) != string(wantOther) {
				t.Fatalf("sibling projection partially refreshed: %q, %v", got, err)
			}

			// Restore the genuine projection for the other table case.
			if err := os.WriteFile(currentPath, beforeCurrent, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keyedPath, beforeKeyed, 0o600); err != nil {
				t.Fatal(err)
			}
		})
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

func TestMaterializeRejectsCaseFoldedProjectionDirectoryBeforeMutation(t *testing.T) {
	workspace := initRepo(t)
	if out, err := exec.Command("git", "-C", workspace, "config", "core.ignorecase", "true").CombinedOutput(); err != nil {
		t.Fatalf("set core.ignorecase: %v: %s", err, out)
	}
	upperDir := filepath.Join(workspace, ".AO")
	if err := os.Mkdir(upperDir, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(upperDir, filename)
	foreign := []byte("tracked uppercase contract must survive\n")
	if err := os.WriteFile(target, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", workspace, "add", ".AO/CONTRACT.md").CombinedOutput(); err != nil {
		t.Fatalf("git add uppercase projection: %v: %s", err, out)
	}
	if err := Materialize(context.Background(), workspace, "canonical"); err == nil {
		t.Fatal("case-folded .AO directory unexpectedly allowed projection")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(foreign) {
		t.Fatalf("uppercase foreign contract changed: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(upperDir, ".gitignore")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uppercase directory mutated before rejection: %v", err)
	}
}

func TestMaterializePRRejectsCaseFoldedContractsDirectoryBeforeMutation(t *testing.T) {
	workspace := initRepo(t)
	dir := filepath.Join(workspace, directory)
	upperContracts := filepath.Join(dir, "Contracts")
	if err := os.MkdirAll(upperContracts, 0o750); err != nil {
		t.Fatal(err)
	}
	foreignPath := filepath.Join(upperContracts, "foreign.md")
	foreign := []byte("capitalized child must survive\n")
	if err := os.WriteFile(foreignPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MaterializePR(context.Background(), workspace, "https://github.com/o/r/pull/32", "canonical"); err == nil {
		t.Fatal("case-folded Contracts directory unexpectedly allowed projection")
	}
	got, err := os.ReadFile(foreignPath)
	if err != nil || string(got) != string(foreign) {
		t.Fatalf("capitalized contracts content changed: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".ao mutated before child case-collision rejection: %v", err)
	}
}

func TestMaterializeRejectsTrackedMissingProjectionPathWithoutCreatingDirectory(t *testing.T) {
	workspace := initRepo(t)
	dir := filepath.Join(workspace, directory)
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(dir, "sparse-owned.txt")
	if err := os.WriteFile(owned, []byte("tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", workspace, "add", ".ao/sparse-owned.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add sparse path: %v: %s", err, out)
	}
	if err := os.Remove(owned); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := Materialize(context.Background(), workspace, "canonical"); err == nil {
		t.Fatal("missing tracked .ao path unexpectedly allowed projection")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tracked missing .ao directory was recreated: %v", err)
	}
}

func TestConcurrentMaterializePRWritesCompleteSerializedProjections(t *testing.T) {
	workspace := initRepo(t)
	type input struct {
		pr       string
		contract string
	}
	inputs := []input{
		{"https://github.com/o/r/pull/40", "contract-A-" + strings.Repeat("A", 128*1024)},
		{"https://github.com/o/r/pull/41", "contract-B-" + strings.Repeat("B", 128*1024)},
	}
	start := make(chan struct{})
	errs := make(chan error, len(inputs))
	var wg sync.WaitGroup
	for _, in := range inputs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- MaterializePR(context.Background(), workspace, in.pr, in.contract)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent materialize: %v", err)
		}
	}

	currentRel := filepath.ToSlash(filepath.Join(directory, filename))
	current, err := os.ReadFile(Path(workspace))
	if err != nil {
		t.Fatal(err)
	}
	currentA := projectionContent(currentRel, "Pull request: "+inputs[0].pr, inputs[0].contract)
	currentB := projectionContent(currentRel, "Pull request: "+inputs[1].pr, inputs[1].contract)
	if string(current) != currentA && string(current) != currentB {
		t.Fatalf("current projection is mixed or partial: %d bytes", len(current))
	}
	for _, in := range inputs {
		rel := testKeyedProjectionRelative(in.pr)
		got, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(rel)))
		want := projectionContent(rel, "Pull request: "+in.pr, in.contract)
		if err != nil || string(got) != want {
			t.Fatalf("keyed projection %s is mixed or partial: %d/%d bytes, %v", in.pr, len(got), len(want), err)
		}
	}
}

func TestMaterializeProjectionFailuresAreCrashAtomicAndRecoverable(t *testing.T) {
	prURL := "https://github.com/o/r/pull/151"
	boundaries := []projectionFailureBoundary{
		projectionCreateBoundary,
		projectionWriteBoundary,
		projectionSyncBoundary,
		projectionCloseBoundary,
		projectionReplaceBoundary,
	}
	tests := []struct {
		name       string
		target     func(string) string
		initialize func(*testing.T, string)
		write      func(string, string, *projectionIO) error
		restart    func(string, string) error
	}{
		{
			name:   "fresh gitignore initialization",
			target: func(string) string { return filepath.ToSlash(filepath.Join(directory, ".gitignore")) },
			write: func(workspace, contract string, ops *projectionIO) error {
				return materializeWithProjectionControls(context.Background(), workspace, "Session draft (no PR identity yet)", "", contract, nil, ops)
			},
			restart: func(workspace, contract string) error { return Materialize(context.Background(), workspace, contract) },
		},
		{
			name:   "fresh current projection",
			target: func(string) string { return filepath.ToSlash(filepath.Join(directory, filename)) },
			initialize: func(t *testing.T, workspace string) {
				t.Helper()
				initializeProjectionIgnore(t, workspace)
			},
			write: func(workspace, contract string, ops *projectionIO) error {
				return materializeWithProjectionControls(context.Background(), workspace, "Session draft (no PR identity yet)", "", contract, nil, ops)
			},
			restart: func(workspace, contract string) error { return Materialize(context.Background(), workspace, contract) },
		},
		{
			name:   "refresh current projection",
			target: func(string) string { return filepath.ToSlash(filepath.Join(directory, filename)) },
			initialize: func(t *testing.T, workspace string) {
				t.Helper()
				if err := Materialize(context.Background(), workspace, "previous complete contract"); err != nil {
					t.Fatal(err)
				}
			},
			write: func(workspace, contract string, ops *projectionIO) error {
				return materializeWithProjectionControls(context.Background(), workspace, "Session draft (no PR identity yet)", "", contract, nil, ops)
			},
			restart: func(workspace, contract string) error { return Materialize(context.Background(), workspace, contract) },
		},
		{
			name:   "fresh keyed per-PR projection",
			target: func(string) string { return testKeyedProjectionRelative(prURL) },
			initialize: func(t *testing.T, workspace string) {
				t.Helper()
				initializeProjectionIgnore(t, workspace)
			},
			write: func(workspace, contract string, ops *projectionIO) error {
				return materializeWithProjectionControls(context.Background(), workspace, "Pull request: "+prURL, prURL, contract, nil, ops)
			},
			restart: func(workspace, contract string) error {
				return MaterializePR(context.Background(), workspace, prURL, contract)
			},
		},
		{
			name:   "refresh keyed per-PR projection",
			target: func(string) string { return testKeyedProjectionRelative(prURL) },
			initialize: func(t *testing.T, workspace string) {
				t.Helper()
				if err := MaterializePR(context.Background(), workspace, prURL, "previous complete contract"); err != nil {
					t.Fatal(err)
				}
			},
			write: func(workspace, contract string, ops *projectionIO) error {
				return materializeWithProjectionControls(context.Background(), workspace, "Pull request: "+prURL, prURL, contract, nil, ops)
			},
			restart: func(workspace, contract string) error {
				return MaterializePR(context.Background(), workspace, prURL, contract)
			},
		},
	}

	for _, tc := range tests {
		for _, boundary := range boundaries {
			t.Run(tc.name+"/"+string(boundary), func(t *testing.T) {
				workspace := initRepo(t)
				if tc.initialize != nil {
					tc.initialize(t, workspace)
				}
				targetRel := tc.target(workspace)
				targetPath := filepath.Join(workspace, filepath.FromSlash(targetRel))
				before, beforeErr := os.ReadFile(targetPath)
				injected := false
				ops := failingProjectionIO(boundary, targetRel, &injected)
				contract := "replacement complete contract " + string(boundary)
				if err := tc.write(workspace, contract, &ops); err == nil || !injected {
					t.Fatalf("failure injection = %v, injected=%v", err, injected)
				}

				after, afterErr := os.ReadFile(targetPath)
				if beforeErr == nil {
					if afterErr != nil || string(after) != string(before) {
						t.Fatalf("failed refresh changed complete target: before=%q after=%q err=%v", before, after, afterErr)
					}
				} else if !errors.Is(beforeErr, os.ErrNotExist) || !errors.Is(afterErr, os.ErrNotExist) {
					t.Fatalf("failed fresh write left final target: before=%v after=%q, %v", beforeErr, after, afterErr)
				}
				assertCleanGitStatus(t, workspace)

				restartErr := tc.restart(workspace, contract)
				if restartErr != nil {
					t.Fatalf("restart recovery: %v", restartErr)
				}
				recovered, err := os.ReadFile(targetPath)
				wantRecovered := contract
				if filepath.Base(targetPath) == ".gitignore" {
					wantRecovered = projectionGitignoreContent()
				}
				if err != nil || !strings.Contains(string(recovered), wantRecovered) {
					t.Fatalf("recovered projection = %q, %v", recovered, err)
				}
				current, err := os.ReadFile(Path(workspace))
				if err != nil || !strings.Contains(string(current), contract) {
					t.Fatalf("recovered current projection = %q, %v", current, err)
				}
				assertCleanGitStatus(t, workspace)
			})
		}
	}
}

func TestProjectionReplaceBoundaryRevalidatesTargetIdentity(t *testing.T) {
	workspace := initRepo(t)
	if err := Materialize(context.Background(), workspace, "previous complete contract"); err != nil {
		t.Fatal(err)
	}
	targetRel := filepath.ToSlash(filepath.Join(directory, filename))
	targetPath := filepath.Join(workspace, filepath.FromSlash(targetRel))
	movedPath := filepath.Join(workspace, directory, "previous-owned.md")
	foreign := []byte("foreign path-swap bytes must survive\n")
	hook := func(boundary projectionFailureBoundary, target string) error {
		if boundary != projectionReplaceBoundary || target != targetRel {
			return nil
		}
		if err := os.Rename(targetPath, movedPath); err != nil {
			return err
		}
		return os.WriteFile(targetPath, foreign, 0o600)
	}
	err := materializeWithFailureHook(context.Background(), workspace, "replacement contract", hook)
	if err == nil {
		t.Fatal("foreign replace-boundary swap unexpectedly allowed refresh")
	}
	got, readErr := os.ReadFile(targetPath)
	if readErr != nil || string(got) != string(foreign) {
		t.Fatalf("foreign replacement changed: %q, %v", got, readErr)
	}
}

func TestFreshProjectionPublishIsBoundToValidatedStageHandle(t *testing.T) {
	workspace := initRepo(t)
	initializeProjectionIgnore(t, workspace)
	targetRel := filepath.ToSlash(filepath.Join(directory, filename))
	foreignSource := filepath.Join(t.TempDir(), "foreign-stage-source")
	foreign := []byte("foreign stage bytes must survive\n")
	if err := os.WriteFile(foreignSource, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	hookRan := false
	hook := func(boundary projectionFailureBoundary, target string) error {
		if boundary != projectionPublishBoundary || target != targetRel {
			return nil
		}
		hookRan = true
		stage := findProjectionStage(t, filepath.Join(workspace, directory, gitignoreStageDirectory))
		moved := filepath.Join(filepath.Dir(stage), "owned-stage-moved")
		if err := os.Rename(stage, moved); err != nil {
			if runtime.GOOS == "windows" {
				return err // the no-share-delete stage handle is the guarantee
			}
			t.Fatal(err)
		}
		return os.WriteFile(stage, foreign, 0o600)
	}
	err := materializeWithFailureHook(context.Background(), workspace, "complete canonical", hook)
	if !hookRan {
		t.Fatal("post-validation publish seam was not reached")
	}
	if runtime.GOOS == "windows" {
		if err == nil {
			t.Fatal("Windows stage pathname swap unexpectedly succeeded")
		}
		if _, statErr := os.Stat(Path(workspace)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("failed stage swap published a final projection: %v", statErr)
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		got, readErr := os.ReadFile(Path(workspace))
		if readErr != nil || !strings.Contains(string(got), "complete canonical") {
			t.Fatalf("handle-bound fresh projection = %q, %v", got, readErr)
		}
		stage := findForeignProjectionStage(t, filepath.Join(workspace, directory, gitignoreStageDirectory), foreign)
		if got, readErr := os.ReadFile(stage); readErr != nil || string(got) != string(foreign) {
			t.Fatalf("foreign staging path changed: %q, %v", got, readErr)
		}
	}
	got, readErr := os.ReadFile(foreignSource)
	if readErr != nil || string(got) != string(foreign) {
		t.Fatalf("foreign source changed: %q, %v", got, readErr)
	}
}

func TestGitignoreBootstrapPublishIsBoundToValidatedStageHandle(t *testing.T) {
	workspace := initRepo(t)
	targetRel := filepath.ToSlash(filepath.Join(directory, ".gitignore"))
	foreign := []byte("foreign bootstrap stage must survive\n")
	hookRan := false
	hook := func(boundary projectionFailureBoundary, target string) error {
		if boundary != projectionPublishBoundary || target != targetRel {
			return nil
		}
		hookRan = true
		stageDir := filepath.Join(workspace, directory, gitignoreStageDirectory)
		entries, err := os.ReadDir(stageDir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !isGitignoreStagePayload(entry.Name()) {
				continue
			}
			stage := filepath.Join(stageDir, entry.Name())
			if err := os.Rename(stage, filepath.Join(stageDir, "owned-bootstrap-moved")); err != nil {
				return err
			}
			return os.WriteFile(stage, foreign, 0o600)
		}
		return errors.New("bootstrap payload not found")
	}
	err := materializeWithFailureHook(context.Background(), workspace, "canonical", hook)
	if !hookRan {
		t.Fatal("bootstrap publish seam was not reached")
	}
	if runtime.GOOS == "windows" {
		if err == nil {
			t.Fatal("Windows bootstrap stage swap unexpectedly succeeded")
		}
		if _, statErr := os.Stat(filepath.Join(workspace, directory, ".gitignore")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("failed bootstrap swap installed gitignore: %v", statErr)
		}
		return
	}
	if err == nil {
		t.Fatal("post-validation foreign entry unexpectedly allowed the full materialization")
	}
	got, readErr := os.ReadFile(filepath.Join(workspace, directory, ".gitignore"))
	if readErr != nil || string(got) != projectionGitignoreContent() {
		t.Fatalf("handle-bound gitignore = %q, %v", got, readErr)
	}
	stageDir := filepath.Join(workspace, directory, gitignoreStageDirectory)
	if findForeignProjectionStage(t, stageDir, foreign) == "" {
		t.Fatal("foreign bootstrap stage disappeared")
	}
}

func TestFreshProjectionPublishNeverReplacesAppearingTarget(t *testing.T) {
	workspace := initRepo(t)
	initializeProjectionIgnore(t, workspace)
	targetRel := filepath.ToSlash(filepath.Join(directory, filename))
	foreign := []byte("foreign final bytes must survive\n")
	hook := func(boundary projectionFailureBoundary, target string) error {
		if boundary == projectionPublishBoundary && target == targetRel {
			return os.WriteFile(Path(workspace), foreign, 0o600)
		}
		return nil
	}
	if err := materializeWithFailureHook(context.Background(), workspace, "canonical", hook); err == nil {
		t.Fatal("appearing foreign target unexpectedly replaced")
	}
	got, err := os.ReadFile(Path(workspace))
	if err != nil || string(got) != string(foreign) {
		t.Fatalf("appearing foreign target changed: %q, %v", got, err)
	}
}

func TestRefreshPublishLocksExactTargetOrFailsClosed(t *testing.T) {
	workspace := initRepo(t)
	if err := Materialize(context.Background(), workspace, "previous complete contract"); err != nil {
		t.Fatal(err)
	}
	targetRel := filepath.ToSlash(filepath.Join(directory, filename))
	foreignSource := filepath.Join(t.TempDir(), "foreign-refresh-source")
	foreign := []byte("foreign refresh bytes must survive\n")
	if err := os.WriteFile(foreignSource, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	hookRan := false
	hook := func(boundary projectionFailureBoundary, target string) error {
		if boundary != projectionPublishBoundary || target != targetRel {
			return nil
		}
		hookRan = true
		return os.Rename(Path(workspace), filepath.Join(workspace, directory, "swapped-target"))
	}
	err := materializeWithFailureHook(context.Background(), workspace, "replacement", hook)
	if err == nil {
		t.Fatal("refresh swap unexpectedly succeeded")
	}
	if !hookRan {
		t.Fatal("conditional refresh did not reach the final publish seam")
	}
	oldPath := Path(workspace)
	if runtime.GOOS != "windows" {
		oldPath = filepath.Join(workspace, directory, "swapped-target")
	}
	got, readErr := os.ReadFile(oldPath)
	if readErr != nil || !strings.Contains(string(got), "previous complete contract") {
		t.Fatalf("validated old target changed: %q, %v", got, readErr)
	}
	got, readErr = os.ReadFile(foreignSource)
	if readErr != nil || string(got) != string(foreign) {
		t.Fatalf("foreign refresh source changed: %q, %v", got, readErr)
	}
}

func findProjectionStage(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".CONTRACT-") && strings.HasSuffix(entry.Name(), ".tmp") {
			return filepath.Join(dir, entry.Name())
		}
	}
	t.Fatal("projection stage not found")
	return ""
}

func findForeignProjectionStage(t *testing.T, dir string, foreign []byte) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if got, err := os.ReadFile(path); err == nil && string(got) == string(foreign) {
			return path
		}
	}
	t.Fatal("foreign projection stage not found")
	return ""
}

func initializeProjectionIgnore(t *testing.T, workspace string) {
	t.Helper()
	dir := filepath.Join(workspace, directory)
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(projectionGitignoreContent()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertCleanGitStatus(t *testing.T, workspace string) {
	t.Helper()
	out, err := exec.Command("git", "-C", workspace, "status", "--porcelain", "--untracked-files=all").CombinedOutput()
	if err != nil || len(out) != 0 {
		t.Fatalf("projection staging dirtied repo: %v: %q", err, out)
	}
}

func failingProjectionIO(boundary projectionFailureBoundary, target string, injected *bool) projectionIO {
	ops := defaultProjectionIO()
	inject := func(gotTarget string) bool {
		if !*injected && gotTarget == target {
			*injected = true
			return true
		}
		return false
	}
	switch boundary {
	case projectionCreateBoundary:
		base := ops.openStage
		ops.openStage = func(root *os.Root, name, gotTarget string) (*os.File, error) {
			file, err := base(root, name, gotTarget)
			if err == nil && inject(gotTarget) {
				_ = file.Close()
				return nil, errors.New("injected create result failure")
			}
			return file, err
		}
	case projectionWriteBoundary:
		ops.write = func(file *os.File, content []byte, gotTarget string) (int, error) {
			if inject(gotTarget) {
				partial := len(content) / 2
				if partial == 0 {
					partial = 1
				}
				n, err := file.Write(content[:partial])
				if err != nil {
					return n, err
				}
				return n, errors.New("injected partial write failure")
			}
			return file.Write(content)
		}
	case projectionSyncBoundary:
		ops.sync = func(_ *os.File, gotTarget string) error {
			if inject(gotTarget) {
				return errors.New("injected sync operation failure")
			}
			return nil
		}
	case projectionCloseBoundary:
		ops.close = func(file *os.File, gotTarget string) error {
			err := file.Close()
			if err == nil && inject(gotTarget) {
				return errors.New("injected close operation failure")
			}
			return err
		}
	case projectionReplaceBoundary:
		ops.publish = func(_ *os.Root, _ *os.Root, _ *os.File, _ os.FileInfo, _, _ string, _ os.FileInfo, _ func() error, gotTarget string) error {
			if inject(gotTarget) {
				return errors.New("injected publish primitive failure")
			}
			return errors.New("unexpected non-target publish")
		}
	}
	return ops
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
	secondErr := MaterializePR(context.Background(), workspace, prB, "invariant-B")
	if secondErr != nil {
		t.Fatal(secondErr)
	}
	current, err := os.ReadFile(Path(workspace))
	wantCurrent, rejectCurrent := prB, "invariant-A"
	if err != nil || !strings.Contains(string(current), "Scope: Pull request: "+wantCurrent) || strings.Contains(string(current), rejectCurrent) {
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

func testKeyedProjectionPath(workspace, prURL string) string {
	return filepath.Join(workspace, filepath.FromSlash(testKeyedProjectionRelative(prURL)))
}

func testKeyedProjectionRelative(prURL string) string {
	sum := sha256.Sum256([]byte(prURL))
	return filepath.ToSlash(filepath.Join(directory, "contracts", fmt.Sprintf("%x.md", sum[:])))
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

func linkTestDirectoryInRoot(t *testing.T, workspace, relativeTarget, link string) {
	t.Helper()
	if err := os.Symlink(relativeTarget, link); err == nil {
		return
	} else if runtime.GOOS != "windows" {
		t.Skipf("creating in-root directory symlink: %v", err)
	} else {
		target := filepath.Join(workspace, relativeTarget)
		cmd := exec.Command("cmd", "/c", "mklink", "/J", link, target)
		if out, junctionErr := cmd.CombinedOutput(); junctionErr != nil {
			t.Skipf("creating in-root symlink or junction: symlink: %v; junction: %v: %s", err, junctionErr, out)
		}
	}
}

func linkTestFile(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	} else if runtime.GOOS != "windows" {
		t.Skipf("creating file symlink: %v", err)
	} else {
		cmd := exec.Command("cmd", "/c", "mklink", link, target)
		if out, linkErr := cmd.CombinedOutput(); linkErr != nil {
			t.Skipf("creating file symlink: symlink: %v; mklink: %v: %s", err, linkErr, out)
		}
	}
}
