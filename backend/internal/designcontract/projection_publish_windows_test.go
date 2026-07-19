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

func TestWindowsPublishReportsPostRenameFlushFailureWithCompleteFinal(t *testing.T) {
	root, stage, stageInfo := windowsPublishFixture(t, false)
	original := windowsProjectionAPI.flushFileBuffers
	calls := 0
	windowsProjectionAPI.flushFileBuffers = func(handle windows.Handle) error {
		calls++
		if calls == 2 {
			return errors.New("injected published-file flush")
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

func TestWindowsPublishSurfacesHandleRenameFailureWithoutTarget(t *testing.T) {
	root, stage, stageInfo := windowsPublishFixture(t, false)
	original := windowsProjectionAPI.ntSetInformationFile
	windowsProjectionAPI.ntSetInformationFile = func(windows.Handle, *windows.IO_STATUS_BLOCK, *byte, uint32, uint32) error {
		return errors.New("injected handle rename")
	}
	t.Cleanup(func() { windowsProjectionAPI.ntSetInformationFile = original })

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

func TestWindowsRefreshDispositionFailurePreservesOldTarget(t *testing.T) {
	root, stage, stageInfo := windowsPublishFixture(t, true)
	targetInfo, err := root.Lstat("target.md")
	if err != nil {
		t.Fatal(err)
	}
	original := windowsProjectionAPI.setFileInformationByHandle
	windowsProjectionAPI.setFileInformationByHandle = func(windows.Handle, uint32, *byte, uint32) error {
		return errors.New("injected exact-target disposition")
	}
	t.Cleanup(func() { windowsProjectionAPI.setFileInformationByHandle = original })

	if err := publishProjectionFile(root, root, stage, stageInfo, "stage.tmp", "target.md", targetInfo, func() error { return nil }); err == nil {
		t.Fatal("target disposition failure was not reported")
	}
	got, readErr := root.ReadFile("target.md")
	if readErr != nil || string(got) != "previous complete bytes" {
		t.Fatalf("old target changed: %q, %v", got, readErr)
	}
}

func TestWindowsRefreshRenameFailureLeavesNoPartialFinal(t *testing.T) {
	root, stage, stageInfo := windowsPublishFixture(t, true)
	targetInfo, err := root.Lstat("target.md")
	if err != nil {
		t.Fatal(err)
	}
	original := windowsProjectionAPI.ntSetInformationFile
	windowsProjectionAPI.ntSetInformationFile = func(windows.Handle, *windows.IO_STATUS_BLOCK, *byte, uint32, uint32) error {
		return errors.New("injected refresh handle rename")
	}
	t.Cleanup(func() { windowsProjectionAPI.ntSetInformationFile = original })

	if err := publishProjectionFile(root, root, stage, stageInfo, "stage.tmp", "target.md", targetInfo, func() error { return nil }); err == nil {
		t.Fatal("refresh handle rename failure was not reported")
	}
	if _, err := root.Lstat("target.md"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed refresh left a partial final: %v", err)
	}
	got, err := root.ReadFile("stage.tmp")
	if err != nil || string(got) != "complete staged bytes" {
		t.Fatalf("failed refresh changed recoverable stage: %q, %v", got, err)
	}
}

func TestWindowsRefreshNoReplacePreservesTargetAppearingAtRenameSyscall(t *testing.T) {
	root, stage, stageInfo := windowsPublishFixture(t, true)
	targetInfo, err := root.Lstat("target.md")
	if err != nil {
		t.Fatal(err)
	}
	foreign := []byte("foreign target appearing after exact target deletion")
	original := windowsProjectionAPI.ntSetInformationFile
	windowsProjectionAPI.ntSetInformationFile = func(source windows.Handle, status *windows.IO_STATUS_BLOCK, info *byte, size, class uint32) error {
		if err := root.WriteFile("target.md", foreign, 0o600); err != nil {
			return err
		}
		return original(source, status, info, size, class)
	}
	t.Cleanup(func() { windowsProjectionAPI.ntSetInformationFile = original })

	if err := publishProjectionFile(root, root, stage, stageInfo, "stage.tmp", "target.md", targetInfo, func() error { return nil }); err == nil {
		t.Fatal("appearing target unexpectedly replaced at handle-rename syscall")
	}
	got, err := root.ReadFile("target.md")
	if err != nil || string(got) != string(foreign) {
		t.Fatalf("appearing foreign target changed: %q, %v", got, err)
	}
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
