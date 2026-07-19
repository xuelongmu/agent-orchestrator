//go:build linux

package designcontract

import (
	"errors"
	"os"
	"testing"
)

func TestLinuxLinkFailureLeavesFreshFinalAbsentAndStageRecoverable(t *testing.T) {
	root, stage, stageInfo := posixPublishFixture(t)
	original := linuxProjectionLinkat
	linuxProjectionLinkat = func(int, string, int, string, int) error {
		return errors.New("injected linkat outcome")
	}
	t.Cleanup(func() { linuxProjectionLinkat = original })

	if err := publishProjectionFile(root, root, stage, stageInfo, "stage.tmp", "target.md", nil, func() error { return nil }); err == nil {
		t.Fatal("linkat failure was not reported")
	}
	if _, err := root.Lstat("target.md"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed linkat published a target: %v", err)
	}
	if got, err := root.ReadFile("stage.tmp"); err != nil || string(got) != "complete staged bytes" {
		t.Fatalf("failed linkat changed stage: %q, %v", got, err)
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
