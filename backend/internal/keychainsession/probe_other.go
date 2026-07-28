//go:build !darwin

package keychainsession

import "context"

// Probe reports unsupported away from macOS, where this audit-session failure
// mode and the `security` CLI do not exist.
func Probe(context.Context) Result {
	return Result{Detail: "macOS login-keychain diagnostic is not applicable"}
}
