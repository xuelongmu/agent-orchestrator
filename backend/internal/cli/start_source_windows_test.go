//go:build windows

package cli

import (
	"reflect"
	"testing"
)

func TestDevHarnessArgv_Windows(t *testing.T) {
	tests := []struct {
		name     string
		npmPath  string
		wantName string
		wantArgs []string
	}{
		{
			// CreateProcess cannot execute a batch file, so the shim npm ships as
			// must be handed to the command interpreter.
			name:     "cmd shim goes through cmd.exe",
			npmPath:  `C:\Program Files\nodejs\npm.cmd`,
			wantName: "cmd.exe",
			wantArgs: []string{"/c", `C:\Program Files\nodejs\npm.cmd`, "run", "dev"},
		},
		{
			name:     "bat shim goes through cmd.exe",
			npmPath:  `C:\tools\npm.BAT`,
			wantName: "cmd.exe",
			wantArgs: []string{"/c", `C:\tools\npm.BAT`, "run", "dev"},
		},
		{
			name:     "real executable runs directly",
			npmPath:  `C:\Program Files\nodejs\npm.exe`,
			wantName: `C:\Program Files\nodejs\npm.exe`,
			wantArgs: []string{"run", "dev"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotArgs := devHarnessArgv(tt.npmPath)
			if gotName != tt.wantName || !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Fatalf("devHarnessArgv(%q) = %q %v, want %q %v", tt.npmPath, gotName, gotArgs, tt.wantName, tt.wantArgs)
			}
		})
	}
}
