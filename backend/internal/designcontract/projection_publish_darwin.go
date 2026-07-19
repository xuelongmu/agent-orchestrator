//go:build darwin

package designcontract

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

var darwinProjectionClonefileat = unix.Fclonefileat
var darwinProjectionRenameat = unix.Renameat
var darwinProjectionRenameatxNp = unix.RenameatxNp

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
		if err := ensureOpenedFileStillBound(sourceRoot, stageName, stageIdentity); err != nil {
			return fmt.Errorf("final staging identity validation: %w", err)
		}
		if err := ensureOpenedFileStillBound(targetRoot, targetName, targetIdentity); err != nil {
			return fmt.Errorf("final target identity validation: %w", err)
		}
		if err := darwinProjectionRenameat(int(sourceDir.Fd()), stageName, int(targetDir.Fd()), targetName); err != nil {
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
	if err := beforePublish(); err != nil {
		return err
	}
	dir, err := targetRoot.Open(".")
	if err != nil {
		return fmt.Errorf("open projection publish directory: %w", err)
	}
	defer func() { _ = dir.Close() }()
	// fclonefileat binds the source to the validated descriptor and creates the
	// destination with no replacement. Unsupported filesystems fail closed.
	if err := darwinProjectionClonefileat(int(stageFile.Fd()), int(dir.Fd()), targetName, 0); err != nil {
		return fmt.Errorf("handle-clone fresh design contract projection: %w", err)
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
	if err := darwinProjectionRenameatxNp(int(sourceDir.Fd()), stageName, int(targetDir.Fd()), targetName, unix.RENAME_EXCL); err != nil {
		return err
	}
	if err := sourceDir.Sync(); err != nil {
		return err
	}
	return targetDir.Sync()
}
