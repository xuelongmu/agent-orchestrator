//go:build !windows

package verification

func validatePlatformExecutable(string) error { return nil }
