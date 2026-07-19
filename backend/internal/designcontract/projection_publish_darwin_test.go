//go:build darwin

package designcontract

import (
	"errors"
	"os"
	"testing"
)

func TestDarwinCloneFailureLeavesFreshFinalAbsentAndStageRecoverable(t *testing.T) {
	root, stage, stageInfo := posixPublishFixture(t)
	original := darwinProjectionClonefileat
	darwinProjectionClonefileat = func(int, int, string, int) error {
		return errors.New("injected fclonefileat outcome")
	}
	t.Cleanup(func() { darwinProjectionClonefileat = original })

	if err := publishProjectionFile(root, root, stage, stageInfo, "stage.tmp", "target.md", nil, func() error { return nil }); err == nil {
		t.Fatal("fclonefileat failure was not reported")
	}
	if _, err := root.Lstat("target.md"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed fclonefileat published a target: %v", err)
	}
	if got, err := root.ReadFile("stage.tmp"); err != nil || string(got) != "complete staged bytes" {
		t.Fatalf("failed fclonefileat changed stage: %q, %v", got, err)
	}
}

func posixPublishFixture(t *testing.T) (*os.Root, *os.File, os.FileInfo) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(dir+string(os.PathSeparator)+"stage.tmp", []byte("complete staged bytes"), 0o600); err != nil {
		t.Fatal(err)
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
