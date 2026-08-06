//go:build windows

package conpty

import (
	"math"
	"strings"
	"testing"
)

func TestDefaultOSProcessFinderRejectsPIDOutsideWindowsRange(t *testing.T) {
	_, err := defaultOSProcessFinder(int(uint64(math.MaxUint32) + 1))
	if err == nil || !strings.Contains(err.Error(), "invalid pid") {
		t.Fatalf("defaultOSProcessFinder error = %v, want invalid pid", err)
	}
}
