//go:build windows

package designcontract

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

var windowsProjectionAPI = struct {
	createFile                 func(*uint16, uint32, uint32, *windows.SecurityAttributes, uint32, uint32, windows.Handle) (windows.Handle, error)
	setFileInformationByHandle func(windows.Handle, uint32, *byte, uint32) error
	moveFileEx                 func(*uint16, *uint16, uint32) error
	flushFileBuffers           func(windows.Handle) error
}{
	createFile:                 windows.CreateFile,
	setFileInformationByHandle: windows.SetFileInformationByHandle,
	moveFileEx:                 windows.MoveFileEx,
	flushFileBuffers:           windows.FlushFileBuffers,
}

func publishProjectionFile(sourceRoot, targetRoot *os.Root, _ *os.File, stageIdentity os.FileInfo, stageName, targetName string, targetIdentity os.FileInfo, beforePublish func() error) error {
	stage, _, err := openLockedWindowsProjectionFile(sourceRoot, stageName, windows.GENERIC_READ, stageIdentity)
	if err != nil {
		return fmt.Errorf("lock design contract staging inode: %w", err)
	}
	defer func() { _ = stage.Close() }()

	directory, directoryHandle, err := openDurableWindowsDirectory(targetRoot)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	// Probe durability support before any namespace mutation. Filesystems that
	// cannot flush directory metadata fail closed rather than receiving an
	// atomicity claim based on NTFS-specific behavior.
	if err := windowsProjectionAPI.flushFileBuffers(directoryHandle); err != nil {
		return fmt.Errorf("projection filesystem lacks durable directory flush: %w", err)
	}

	var target *os.File
	if targetIdentity != nil {
		target, _, err = openLockedWindowsProjectionFile(targetRoot, targetName, windows.GENERIC_READ, targetIdentity)
		if err != nil {
			return fmt.Errorf("lock exact design contract target inode: %w", err)
		}
		defer func(file *os.File) { _ = file.Close() }(target)
		if err := ensureOpenedFileStillBound(sourceRoot, stageName, stageIdentity); err != nil {
			return err
		}
		if err := ensureOpenedFileStillBound(targetRoot, targetName, targetIdentity); err != nil {
			return err
		}
		if err := beforePublish(); err != nil {
			return err
		}
		if err := target.Close(); err != nil {
			return fmt.Errorf("close exact design contract target lock: %w", err)
		}
	} else {
		if _, err := targetRoot.Lstat(targetName); !errors.Is(err, os.ErrNotExist) {
			return errors.New("design contract target appeared before no-replace Windows publish")
		}
		if err := beforePublish(); err != nil {
			return err
		}
	}
	if err := stage.Close(); err != nil {
		return fmt.Errorf("close exact design contract staging lock: %w", err)
	}
	sourcePath, err := windows.UTF16PtrFromString(filepath.Join(sourceRoot.Name(), filepath.FromSlash(stageName)))
	if err != nil {
		return err
	}
	targetPath, err := windows.UTF16PtrFromString(filepath.Join(targetRoot.Name(), filepath.FromSlash(targetName)))
	if err != nil {
		return err
	}
	flags := uint32(windows.MOVEFILE_WRITE_THROUGH)
	if targetIdentity != nil {
		flags |= windows.MOVEFILE_REPLACE_EXISTING
	}
	// MoveFileExW performs the only namespace mutation. With replacement it is
	// an atomic old-or-new transition; the old target is never deleted first.
	if err := windowsProjectionAPI.moveFileEx(sourcePath, targetPath, flags); err != nil {
		return fmt.Errorf("atomic Windows design contract publish: %w", err)
	}
	if err := windowsProjectionAPI.flushFileBuffers(directoryHandle); err != nil {
		return fmt.Errorf("flush published design contract directory entry: %w", err)
	}
	return nil
}

func publishProjectionDirectory(sourceRoot, targetRoot *os.Root, stageName, targetName string, stageIdentity os.FileInfo) error {
	stagePath := filepath.Join(sourceRoot.Name(), filepath.FromSlash(stageName))
	path, err := windows.UTF16PtrFromString(stagePath)
	if err != nil {
		return err
	}
	handle, err := windowsProjectionAPI.createFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	stage := os.NewFile(uintptr(handle), stagePath)
	info, statErr := stage.Stat()
	if statErr != nil || !info.IsDir() || !os.SameFile(info, stageIdentity) {
		_ = stage.Close()
		return errors.New("opened Windows staging directory does not match validated identity")
	}
	if current, err := sourceRoot.Lstat(stageName); err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, info) {
		_ = stage.Close()
		return errors.New("Windows staging directory changed before no-replace publish")
	}
	sourceDirectory, sourceDirectoryHandle, err := openDurableWindowsDirectory(sourceRoot)
	if err != nil {
		_ = stage.Close()
		return err
	}
	defer func() { _ = sourceDirectory.Close() }()
	targetDirectory, targetDirectoryHandle, err := openDurableWindowsDirectory(targetRoot)
	if err != nil {
		_ = stage.Close()
		return err
	}
	defer func() { _ = targetDirectory.Close() }()
	if err := windowsProjectionAPI.flushFileBuffers(sourceDirectoryHandle); err != nil {
		_ = stage.Close()
		return err
	}
	if err := windowsProjectionAPI.flushFileBuffers(targetDirectoryHandle); err != nil {
		_ = stage.Close()
		return err
	}
	if err := stage.Close(); err != nil {
		return err
	}
	targetPath, err := windows.UTF16PtrFromString(filepath.Join(targetRoot.Name(), filepath.FromSlash(targetName)))
	if err != nil {
		return err
	}
	if err := windowsProjectionAPI.moveFileEx(path, targetPath, windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return err
	}
	if err := windowsProjectionAPI.flushFileBuffers(sourceDirectoryHandle); err != nil {
		return err
	}
	return windowsProjectionAPI.flushFileBuffers(targetDirectoryHandle)
}

func openLockedWindowsProjectionFile(root *os.Root, name string, access uint32, expected os.FileInfo) (*os.File, windows.Handle, error) {
	abs := filepath.Join(root.Name(), filepath.FromSlash(name))
	path, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return nil, 0, err
	}
	// Share only reads: while this handle is live, no peer can rewrite, delete,
	// or rename the validated inode between the final check and publication.
	handle, err := windowsProjectionAPI.createFile(path, access, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, 0, err
	}
	file := os.NewFile(uintptr(handle), abs)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(info, expected) {
		_ = file.Close()
		return nil, 0, errors.New("opened Windows file does not match validated projection identity")
	}
	return file, handle, nil
}

func openDurableWindowsDirectory(root *os.Root) (*os.File, windows.Handle, error) {
	path, err := windows.UTF16PtrFromString(root.Name())
	if err != nil {
		return nil, 0, err
	}
	handle, err := windowsProjectionAPI.createFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_WRITE_THROUGH, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("open durable projection directory handle: %w", err)
	}
	file := os.NewFile(uintptr(handle), root.Name())
	wantFile, err := root.Open(".")
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	want, wantErr := wantFile.Stat()
	_ = wantFile.Close()
	got, gotErr := file.Stat()
	if wantErr != nil || gotErr != nil || !got.IsDir() || !os.SameFile(want, got) {
		_ = file.Close()
		return nil, 0, errors.New("durable Windows directory handle does not match verified projection root")
	}
	return file, handle, nil
}
