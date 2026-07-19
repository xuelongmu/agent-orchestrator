//go:build !windows

package designcontract

import "os"

func syncProjectionContainer(root *os.Root) error {
	return syncProjectionDirectory(root, "")
}
