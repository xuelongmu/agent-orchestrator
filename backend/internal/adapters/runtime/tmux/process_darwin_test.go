//go:build darwin

package tmux

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestDarwinFailsClosedInsteadOfNumericPIDDelivery(t *testing.T) {
	table := osProcessTable{runner: execRunner{}, timeout: defaultTimeout}
	observation, err := table.Open(context.Background(), os.Getpid())
	if err == nil {
		closeObservation(observation)
		t.Fatal("Open unexpectedly returned a delivery handle; Darwin kill(2) would re-resolve a reusable numeric PID")
	}
	if !strings.Contains(err.Error(), "exact process signal handles are unavailable on darwin") {
		t.Fatalf("Open error = %v, want explicit fail-closed limitation", err)
	}
}

func TestDarwinReuseAtDeliveryHasNoSignalPath(t *testing.T) {
	// Opening is the only route to process delivery. Refusing it before any
	// kqueue poll/kill sequence makes a reuse in that former gap signal-free.
	if observation, err := platformOpenProcess(4242); err == nil {
		closeObservation(observation)
		t.Fatal("platformOpenProcess unexpectedly exposed numeric-PID delivery")
	}
}
