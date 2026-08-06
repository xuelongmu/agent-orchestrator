//go:build windows

package ptyregistry

import (
	"syscall"
	"unsafe"
)

const movefileReplaceExisting = 0x1

var moveFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func atomicReplace(src, dst string) error {
	srcPtr, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	dstPtr, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	ret, _, callErr := moveFileExW.Call(
		uintptr(unsafe.Pointer(srcPtr)), // #nosec G103 -- required by MoveFileExW.
		uintptr(unsafe.Pointer(dstPtr)), // #nosec G103 -- required by MoveFileExW.
		uintptr(movefileReplaceExisting),
	)
	if ret == 0 {
		return callErr
	}
	return nil
}
