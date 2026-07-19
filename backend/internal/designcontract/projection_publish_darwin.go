//go:build darwin

package designcontract

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

var darwinProjectionClonefileat = unix.Fclonefileat

func publishProjectionFile(sourceRoot, targetRoot *os.Root, stageFile *os.File, stageIdentity os.FileInfo, stageName, targetName string, targetIdentity os.FileInfo, beforePublish func() error) error {
	if targetIdentity != nil {
		return errors.New("atomic design contract projection refresh is unavailable on macOS; canonical SQLite state remains available")
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
