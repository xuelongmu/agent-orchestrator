//go:build windows

package pathenv

import "os/exec"

// Windows process creation has no portable system fallback equivalent to the
// POSIX default; preserve the inherited environment's lookup semantics.
const defaultPATH = ""

// LookPath delegates to the Windows executable search rules unchanged.
func LookPath(file string) (string, error) {
	return exec.LookPath(file)
}
