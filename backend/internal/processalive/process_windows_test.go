//go:build windows

package processalive

import (
	"math"
	"testing"
)

func TestAliveRejectsPIDOutsideWindowsRange(t *testing.T) {
	if Alive(int(uint64(math.MaxUint32) + 1)) {
		t.Fatal("Alive accepted a PID that cannot be represented by Win32")
	}
}
