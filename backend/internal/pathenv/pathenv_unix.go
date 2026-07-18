//go:build !windows

package pathenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const defaultPATH = "/usr/local/bin:/usr/bin:/bin"

// LookPath matches exec.LookPath while preserving the system search path when
// a headless daemon starts without PATH. Passing a path still uses the standard
// library directly, including its executable-file checks.
func LookPath(file string) (string, error) {
	if os.Getenv("PATH") != "" || strings.ContainsRune(file, filepath.Separator) {
		return exec.LookPath(file)
	}
	for _, dir := range filepath.SplitList(defaultPATH) {
		if path, err := exec.LookPath(filepath.Join(dir, file)); err == nil {
			return path, nil
		}
	}
	return "", exec.ErrNotFound
}
