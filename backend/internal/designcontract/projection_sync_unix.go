//go:build !windows

package designcontract

import "os"

// syncProjectionDirectory makes a completed link/rename/removal durable on
// filesystems that require the containing directory to be fsynced separately
// from file contents.
func syncProjectionDirectory(root *os.Root, _ string) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
