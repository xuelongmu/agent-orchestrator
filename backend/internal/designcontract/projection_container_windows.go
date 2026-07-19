//go:build windows

package designcontract

import (
	"os"
)

func syncProjectionContainer(root *os.Root) error {
	directory, handle, err := openDurableWindowsDirectory(root)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return windowsProjectionAPI.flushFileBuffers(handle)
}
