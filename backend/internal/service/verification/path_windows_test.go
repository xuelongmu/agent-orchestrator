//go:build windows

package verification

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsReparsePointRejectsWorkspaceLogJunctionOrSymlink(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("Windows symlink unavailable: %v", err)
	}
	if !isReparsePoint(link) {
		t.Fatal("isReparsePoint returned false for symlink")
	}
}
