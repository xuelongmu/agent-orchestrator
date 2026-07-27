//go:build windows

package cli

import (
	"path/filepath"
	"strings"
)

// devHarnessArgv builds the command that runs the frontend dev harness.
//
// On Windows a normal Node install exposes npm as the batch shim `npm.cmd`.
// CreateProcess — which is what os/exec ultimately calls — does not execute
// batch files itself, so invoking the resolved shim directly can fail with
// "%1 is not a valid Win32 application" even though LookPath found it. Route
// batch shims through the command interpreter explicitly. cmd.exe is started
// attached to the same console, so Ctrl-C still reaches the harness.
//
// npmPath is the LookPath-resolved binary; a non-batch executable is run
// directly, matching the other platforms.
func devHarnessArgv(npmPath string) (string, []string) {
	switch strings.ToLower(filepath.Ext(npmPath)) {
	case ".cmd", ".bat":
		return "cmd.exe", []string{"/c", npmPath, "run", "dev"}
	default:
		return npmPath, []string{"run", "dev"}
	}
}
