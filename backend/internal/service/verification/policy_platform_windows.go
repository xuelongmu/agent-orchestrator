//go:build windows

package verification

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

func validatePlatformExecutable(executable string) error {
	base := strings.ToLower(filepath.Base(executable))
	if base == "npm" || base == "npm.cmd" {
		return nil
	}
	extension := strings.ToLower(filepath.Ext(executable))
	if extension == ".cmd" || extension == ".bat" {
		return fmt.Errorf("Windows batch executable %q is not allowed; configure a native executable", executable)
	}
	if resolved, err := exec.LookPath(executable); err == nil {
		extension = strings.ToLower(filepath.Ext(resolved))
		if extension == ".cmd" || extension == ".bat" {
			return fmt.Errorf("executable %q resolves to Windows batch file %q; configure a native executable", executable, resolved)
		}
	}
	return nil
}
