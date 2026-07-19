//go:build windows

package designcontract

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

var windowsProjectionAPI = struct {
	createFile                 func(*uint16, uint32, uint32, *windows.SecurityAttributes, uint32, uint32, windows.Handle) (windows.Handle, error)
	setFileInformationByHandle func(windows.Handle, uint32, *byte, uint32) error
	ntSetInformationFile       func(windows.Handle, *windows.IO_STATUS_BLOCK, *byte, uint32, uint32) error
	flushFileBuffers           func(windows.Handle) error
}{
	createFile:                 windows.CreateFile,
	setFileInformationByHandle: windows.SetFileInformationByHandle,
	ntSetInformationFile:       windows.NtSetInformationFile,
	flushFileBuffers:           windows.FlushFileBuffers,
}

func publishProjectionFile(sourceRoot, targetRoot *os.Root, _ *os.File, stageIdentity os.FileInfo, stageName, targetName string, targetIdentity os.FileInfo, beforePublish func() error) error {
	stage, stageHandle, err := openLockedWindowsProjectionFile(sourceRoot, stageName, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE, stageIdentity)
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
		var targetHandle windows.Handle
		target, targetHandle, err = openLockedWindowsProjectionFile(targetRoot, targetName, windows.GENERIC_READ|windows.DELETE, targetIdentity)
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
		deleteOnClose := byte(1)
		if err := windowsProjectionAPI.setFileInformationByHandle(targetHandle, windows.FileDispositionInfo, &deleteOnClose, 1); err != nil {
			return fmt.Errorf("mark exact design contract target for deletion: %w", err)
		}
		if err := target.Close(); err != nil {
			return fmt.Errorf("remove exact design contract target inode: %w", err)
		}
	} else {
		if _, err := targetRoot.Lstat(targetName); !errors.Is(err, os.ErrNotExist) {
			return errors.New("design contract target appeared before no-replace Windows publish")
		}
		if err := beforePublish(); err != nil {
			return err
		}
	}

	if err := renameWindowsHandle(stageHandle, directoryHandle, targetName); err != nil {
		return fmt.Errorf("handle-relative no-replace design contract publish: %w", err)
	}
	if err := windowsProjectionAPI.flushFileBuffers(stageHandle); err != nil {
		return fmt.Errorf("flush published design contract inode: %w", err)
	}
	if err := windowsProjectionAPI.flushFileBuffers(directoryHandle); err != nil {
		return fmt.Errorf("flush published design contract directory entry: %w", err)
	}
	return nil
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

func renameWindowsHandle(source, targetDirectory windows.Handle, targetName string) error {
	name, err := windows.UTF16FromString(targetName)
	if err != nil {
		return err
	}
	name = name[:len(name)-1]
	type header struct {
		ReplaceIfExists uint32
		RootDirectory   windows.Handle
		FileNameLength  uint32
		FileName        uint16
	}
	offset := unsafe.Offsetof(header{}.FileName)
	buffer := make([]byte, int(offset)+len(name)*2)
	info := (*header)(unsafe.Pointer(&buffer[0]))
	info.ReplaceIfExists = 0
	info.RootDirectory = targetDirectory
	info.FileNameLength = uint32(len(name) * 2)
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buffer[offset])), len(name)), name)
	var status windows.IO_STATUS_BLOCK
	return windowsProjectionAPI.ntSetInformationFile(source, &status, &buffer[0], uint32(len(buffer)), windows.FileRenameInformation)
}
