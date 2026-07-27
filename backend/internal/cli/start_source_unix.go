//go:build !windows

package cli

// devHarnessArgv builds the command that runs the frontend dev harness. Outside
// Windows npm is an ordinary executable, so the LookPath-resolved path is run
// directly. See start_source_windows.go for the batch-shim handling.
func devHarnessArgv(npmPath string) (string, []string) {
	return npmPath, []string{"run", "dev"}
}
