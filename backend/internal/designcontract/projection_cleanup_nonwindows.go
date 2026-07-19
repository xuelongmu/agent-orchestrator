//go:build !windows

package designcontract

import "os"

// POSIX has no identity-conditional unlink. Stages are already ignored and do
// not block new random stages, so retaining them is the fail-closed behavior.
func cleanupOwnedProjectionStages(*os.Root, string) error { return nil }
