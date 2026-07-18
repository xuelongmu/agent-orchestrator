package pathenv

import (
	"runtime"
	"testing"
)

func TestEffectivePreservesConfiguredPath(t *testing.T) {
	if got := Effective(func(string) string { return "custom-path" }); got != "custom-path" {
		t.Fatalf("Effective() = %q, want custom-path", got)
	}
}

func TestEffectiveUsesPlatformDefaultWhenPathUnset(t *testing.T) {
	want := ""
	if runtime.GOOS != "windows" {
		want = "/usr/local/bin:/usr/bin:/bin"
	}
	if got := Effective(func(string) string { return "" }); got != want {
		t.Fatalf("Effective() = %q, want %q", got, want)
	}
}
