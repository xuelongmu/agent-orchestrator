// Package keychainsession diagnoses whether the daemon and any persistent
// worker runtime can interact with the current user's login keychain.
package keychainsession

// Result is the platform-neutral result returned to the loopback diagnostic
// route. Detail is safe diagnostic prose and never contains secret material.
type Result struct {
	Supported bool
	Available bool
	Detail    string
}
