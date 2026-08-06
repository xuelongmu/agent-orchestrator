//go:build windows

package ptyregistry

import (
	"math"
	"testing"
)

func TestDefaultPidAliveRejectsPIDOutsideWindowsRange(t *testing.T) {
	if defaultPidAlive(int(uint64(math.MaxUint32) + 1)) {
		t.Fatal("defaultPidAlive accepted a PID that cannot be represented by Win32")
	}
}
