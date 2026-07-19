//go:build linux

package designcontract

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

var linuxProjectionLinkat = unix.Linkat

func publishProjectionFile(sourceRoot, targetRoot *os.Root, stageFile *os.File, stageIdentity os.FileInfo, stageName, targetName string, targetIdentity os.FileInfo, beforePublish func() error) error {
	if targetIdentity != nil {
		return errors.New("atomic design contract projection refresh is unavailable on Linux; canonical SQLite state remains available")
	}
	if err := ensureOpenedFileStillBound(sourceRoot, stageName, stageIdentity); err != nil {
		return err
	}
	if _, err := targetRoot.Lstat(targetName); !errors.Is(err, os.ErrNotExist) {
		return errors.New("design contract projection target appeared before no-replace publish")
	}
	dir, err := targetRoot.Open(".")
	if err != nil {
		return fmt.Errorf("open projection publish directory: %w", err)
	}
	defer func() { _ = dir.Close() }()
	// Copy the validated handle into an unnamed inode in the destination
	// directory. Publication therefore creates no persistent alias back to the
	// abandoned named stage and has no source pathname to swap.
	tmpFD, err := unix.Openat(int(dir.Fd()), ".", unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create unnamed design contract publish inode: %w", err)
	}
	tmp := os.NewFile(uintptr(tmpFD), "ao-design-contract-publish")
	defer func() { _ = tmp.Close() }()
	if _, err := stageFile.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(tmp, stageFile); err != nil {
		return fmt.Errorf("copy staged projection into unnamed publish inode: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync unnamed design contract publish inode: %w", err)
	}
	if err := beforePublish(); err != nil {
		return err
	}
	// /proc/self/fd binds linkat to the unnamed open file description. The
	// destination is no-replace, so an appearing foreign target wins safely.
	source := "/proc/self/fd/" + strconv.FormatUint(uint64(tmp.Fd()), 10)
	if err := linuxProjectionLinkat(unix.AT_FDCWD, source, int(dir.Fd()), targetName, unix.AT_SYMLINK_FOLLOW); err != nil {
		return fmt.Errorf("handle-bind fresh design contract projection: %w", err)
	}
	return syncProjectionDirectory(targetRoot, targetName)
}
