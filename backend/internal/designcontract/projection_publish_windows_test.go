//go:build windows

package designcontract

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsPublishFailsBeforeMutationWithoutDirectoryDurability(t *testing.T) {
	root, stage, stageInfo := windowsPublishFixture(t, false)
	original := windowsProjectionAPI.flushFileBuffers
	windowsProjectionAPI.flushFileBuffers = func(windows.Handle) error { return errors.New("injected directory flush") }
	t.Cleanup(func() { windowsProjectionAPI.flushFileBuffers = original })

	err := publishProjectionFile(root, root, stage, stageInfo, "stage.tmp", "target.md", nil, func() error { return nil })
	if err == nil {
		t.Fatal("publish unexpectedly succeeded without durable directory flush")
	}
	if _, err := root.Lstat("stage.tmp"); err != nil {
		t.Fatalf("stage changed before durability probe: %v", err)
	}
	if _, err := root.Lstat("target.md"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target appeared before durability probe: %v", err)
	}
}

func TestWindowsPublishReportsPostMoveDirectoryFlushFailureWithCompleteFinal(t *testing.T) {
	root, stage, stageInfo := windowsPublishFixture(t, false)
	original := windowsProjectionAPI.flushFileBuffers
	calls := 0
	windowsProjectionAPI.flushFileBuffers = func(handle windows.Handle) error {
		calls++
		if calls == 2 {
			return errors.New("injected published-directory flush")
		}
		return original(handle)
	}
	t.Cleanup(func() { windowsProjectionAPI.flushFileBuffers = original })

	err := publishProjectionFile(root, root, stage, stageInfo, "stage.tmp", "target.md", nil, func() error { return nil })
	if err == nil {
		t.Fatal("post-rename durability failure was not reported")
	}
	got, readErr := root.ReadFile("target.md")
	if readErr != nil || string(got) != "complete staged bytes" {
		t.Fatalf("published final is partial: %q, %v", got, readErr)
	}
}

func TestWindowsPublishSurfacesMoveFailureWithoutTarget(t *testing.T) {
	root, stage, stageInfo := windowsPublishFixture(t, false)
	original := windowsProjectionAPI.moveFileEx
	windowsProjectionAPI.moveFileEx = func(*uint16, *uint16, uint32) error {
		return errors.New("injected MoveFileExW")
	}
	t.Cleanup(func() { windowsProjectionAPI.moveFileEx = original })

	if err := publishProjectionFile(root, root, stage, stageInfo, "stage.tmp", "target.md", nil, func() error { return nil }); err == nil {
		t.Fatal("handle rename failure was not reported")
	}
	if _, err := root.Lstat("stage.tmp"); err != nil {
		t.Fatalf("stage changed after failed rename: %v", err)
	}
	if _, err := root.Lstat("target.md"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target appeared after failed rename: %v", err)
	}
}

func TestWindowsRefreshMoveFailurePreservesOldTarget(t *testing.T) {
	root, stage, stageInfo := windowsPublishFixture(t, true)
	targetInfo, err := root.Lstat("target.md")
	if err != nil {
		t.Fatal(err)
	}
	original := windowsProjectionAPI.moveFileEx
	windowsProjectionAPI.moveFileEx = func(*uint16, *uint16, uint32) error {
		return errors.New("injected replacement MoveFileExW")
	}
	t.Cleanup(func() { windowsProjectionAPI.moveFileEx = original })

	if err := publishProjectionFile(root, root, stage, stageInfo, "stage.tmp", "target.md", targetInfo, func() error { return nil }); err == nil {
		t.Fatal("target disposition failure was not reported")
	}
	got, readErr := root.ReadFile("target.md")
	if readErr != nil || string(got) != "previous complete bytes" {
		t.Fatalf("old target changed: %q, %v", got, readErr)
	}
}

func TestWindowsRefreshFailureMatrixLeavesOldOrNewComplete(t *testing.T) {
	root, stage, stageInfo := windowsPublishFixture(t, true)
	targetInfo, err := root.Lstat("target.md")
	if err != nil {
		t.Fatal(err)
	}
	original := windowsProjectionAPI.moveFileEx
	windowsProjectionAPI.moveFileEx = func(*uint16, *uint16, uint32) error {
		return errors.New("injected refresh MoveFileExW")
	}
	t.Cleanup(func() { windowsProjectionAPI.moveFileEx = original })

	if err := publishProjectionFile(root, root, stage, stageInfo, "stage.tmp", "target.md", targetInfo, func() error { return nil }); err == nil {
		t.Fatal("refresh handle rename failure was not reported")
	}
	got, err := root.ReadFile("target.md")
	if err != nil || string(got) != "previous complete bytes" {
		t.Fatalf("failed refresh did not preserve old complete target: %q, %v", got, err)
	}
	got, err = root.ReadFile("stage.tmp")
	if err != nil || string(got) != "complete staged bytes" {
		t.Fatalf("failed refresh changed recoverable stage: %q, %v", got, err)
	}
}

func TestWindowsRefreshInjectedTargetSwapPreservesForeignWhenMoveFails(t *testing.T) {
	root, stage, stageInfo := windowsPublishFixture(t, true)
	targetInfo, err := root.Lstat("target.md")
	if err != nil {
		t.Fatal(err)
	}
	foreign := []byte("foreign target appearing at MoveFileExW boundary")
	original := windowsProjectionAPI.moveFileEx
	windowsProjectionAPI.moveFileEx = func(_, _ *uint16, _ uint32) error {
		if err := os.Rename(filepath.Join(root.Name(), "target.md"), filepath.Join(root.Name(), "validated-old.md")); err != nil {
			return err
		}
		if err := root.WriteFile("target.md", foreign, 0o600); err != nil {
			return err
		}
		return errors.New("injected target swap")
	}
	t.Cleanup(func() { windowsProjectionAPI.moveFileEx = original })

	if err := publishProjectionFile(root, root, stage, stageInfo, "stage.tmp", "target.md", targetInfo, func() error { return nil }); err == nil {
		t.Fatal("appearing target unexpectedly replaced at handle-rename syscall")
	}
	got, err := root.ReadFile("target.md")
	if err != nil || string(got) != string(foreign) {
		t.Fatalf("appearing foreign target changed: %q, %v", got, err)
	}
}

func TestWindowsRefreshUsesOneWriteThroughReplacingMove(t *testing.T) {
	root, stage, stageInfo := windowsPublishFixture(t, true)
	targetInfo, err := root.Lstat("target.md")
	if err != nil {
		t.Fatal(err)
	}
	original := windowsProjectionAPI.moveFileEx
	calls := 0
	windowsProjectionAPI.moveFileEx = func(from, to *uint16, flags uint32) error {
		calls++
		want := uint32(windows.MOVEFILE_REPLACE_EXISTING | windows.MOVEFILE_WRITE_THROUGH)
		if flags != want {
			t.Fatalf("MoveFileExW flags = %#x, want %#x", flags, want)
		}
		return original(from, to, flags)
	}
	t.Cleanup(func() { windowsProjectionAPI.moveFileEx = original })

	if err := publishProjectionFile(root, root, stage, stageInfo, "stage.tmp", "target.md", targetInfo, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("MoveFileExW calls = %d, want 1", calls)
	}
	got, err := root.ReadFile("target.md")
	if err != nil || string(got) != "complete staged bytes" {
		t.Fatalf("replacement target = %q, %v", got, err)
	}
}

func TestWindowsNestedRootNameIsCumulativeAndUsable(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, ".ao", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".ao", ".git", "stage.tmp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	aoRoot, err := root.OpenRoot(".ao")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = aoRoot.Close() }()
	stageRoot, err := aoRoot.OpenRoot(".git")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stageRoot.Close() }()
	if got, want := stageRoot.Name(), filepath.Join(base, ".ao", ".git"); got != want {
		t.Fatalf("nested Root.Name() = %q, want cumulative %q", got, want)
	}
	info, err := stageRoot.Lstat("stage.tmp")
	if err != nil {
		t.Fatal(err)
	}
	file, _, err := openLockedWindowsProjectionFile(stageRoot, "stage.tmp", windows.GENERIC_READ, info)
	if err != nil {
		t.Fatalf("open through nested Root.Name(): %v", err)
	}
	_ = file.Close()
}

func TestWindowsCleanupDeletesOnlyLockedOwnedStage(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	target := filepath.ToSlash(filepath.Join(directory, filename))
	ownedName, foreignName := ".CONTRACT-owned.tmp", ".CONTRACT-foreign.tmp"
	if err := root.WriteFile(ownedName, []byte(projectionOwnershipMarker(target)+"partial body"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("foreign stage bytes\n")
	if err := root.WriteFile(foreignName, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupOwnedProjectionStages(root, target); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat(ownedName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned stage survived handle-bound cleanup: %v", err)
	}
	got, err := root.ReadFile(foreignName)
	if err != nil || string(got) != string(foreign) {
		t.Fatalf("foreign stage changed: %q, %v", got, err)
	}
}

func TestWindowsCleanupPostValidationSwapCannotDeleteForeignPath(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	target := filepath.ToSlash(filepath.Join(directory, filename))
	stage := ".CONTRACT-owned.tmp"
	owned := []byte(projectionOwnershipMarker(target) + "partial body")
	if err := root.WriteFile(stage, owned, 0o600); err != nil {
		t.Fatal(err)
	}
	foreignPath := filepath.Join(dir, "foreign-source")
	foreign := []byte("foreign cleanup bytes\n")
	if err := os.WriteFile(foreignPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	original := windowsCleanupValidatedHook
	windowsCleanupValidatedHook = func(_ *os.Root, _ string) error {
		return os.Rename(filepath.Join(dir, stage), filepath.Join(dir, "owned-moved"))
	}
	t.Cleanup(func() { windowsCleanupValidatedHook = original })
	if err := cleanupOwnedProjectionStages(root, target); err == nil {
		t.Fatal("post-validation cleanup swap unexpectedly succeeded")
	}
	got, err := root.ReadFile(stage)
	if err != nil || string(got) != string(owned) {
		t.Fatalf("owned stage path changed: %q, %v", got, err)
	}
	got, err = os.ReadFile(foreignPath)
	if err != nil || string(got) != string(foreign) {
		t.Fatalf("foreign cleanup state changed: %q, %v", got, err)
	}
}

func windowsPublishFixture(t *testing.T, withTarget bool) (*os.Root, *os.File, os.FileInfo) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stage.tmp"), []byte("complete staged bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if withTarget {
		if err := os.WriteFile(filepath.Join(dir, "target.md"), []byte("previous complete bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	stage, err := root.OpenFile("stage.tmp", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stage.Close() })
	info, err := stage.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return root, stage, info
}
