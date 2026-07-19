//go:build !windows

package verification

func isReparsePoint(string) bool { return false }
