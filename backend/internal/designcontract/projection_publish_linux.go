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
var linuxProjectionRenameat = unix.Renameat
var linuxProjectionRenameat2 = unix.Renameat2

func publishProjectionFile(sourceRoot, targetRoot *os.Root, stageFile *os.File, stageIdentity os.FileInfo, stageName, targetName string, targetIdentity os.FileInfo, beforePublish func() error) error {
	if targetIdentity != nil {
		sourceDir, err := sourceRoot.Open(".")
		if err != nil {
			return fmt.Errorf("open projection staging directory: %w", err)
		}
		defer func() { _ = sourceDir.Close() }()
		targetDir, err := targetRoot.Open(".")
		if err != nil {
			return fmt.Errorf("open projection target directory: %w", err)
		}
		defer func() { _ = targetDir.Close() }()
		if err := beforePublish(); err != nil {
			return err
		}
		// Both names are revalidated through their confined roots at the final
		// syscall boundary. renameat then replaces the exact validated target
		// atomically; a failure leaves the old complete target in place.
		if err := ensureOpenedFileStillBound(sourceRoot, stageName, stageIdentity); err != nil {
			return fmt.Errorf("final staging identity validation: %w", err)
		}
		if err := ensureOpenedFileStillBound(targetRoot, targetName, targetIdentity); err != nil {
			return fmt.Errorf("final target identity validation: %w", err)
		}
		if err := linuxProjectionRenameat(int(sourceDir.Fd()), stageName, int(targetDir.Fd()), targetName); err != nil {
			return fmt.Errorf("atomic design contract projection refresh: %w", err)
		}
		if err := stageFile.Sync(); err != nil {
			return fmt.Errorf("sync refreshed design contract projection: %w", err)
		}
		return targetDir.Sync()
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

func publishProjectionDirectory(sourceRoot, targetRoot *os.Root, stageName, targetName string, stageIdentity os.FileInfo) error {
	sourceDir, err := sourceRoot.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = sourceDir.Close() }()
	targetDir, err := targetRoot.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = targetDir.Close() }()
	current, err := sourceRoot.Lstat(stageName)
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, stageIdentity) {
		return errors.New("gitignore staging directory changed before no-replace publish")
	}
	if err := linuxProjectionRenameat2(int(sourceDir.Fd()), stageName, int(targetDir.Fd()), targetName, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if err := sourceDir.Sync(); err != nil {
		return err
	}
	return targetDir.Sync()
}
