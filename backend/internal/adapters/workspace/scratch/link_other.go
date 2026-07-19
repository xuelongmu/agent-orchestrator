//go:build !windows

package scratch

import "os"

func isLinkLike(info os.FileInfo) bool {
	return info.Mode()&os.ModeSymlink != 0
}
