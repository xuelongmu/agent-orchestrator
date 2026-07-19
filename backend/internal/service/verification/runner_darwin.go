//go:build darwin

package verification

import "syscall"

func verificationSysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// Darwin has no public, unprivileged equivalent of a Linux child subreaper or
// Windows Job Object. In particular, XNU rejects EVFILT_PROC NOTE_TRACK, so a
// guardian cannot race-freely retain ownership after a descendant reparents.
// Keep the process-group guarantee and make the stronger limitation explicit
// in docs/verification.md and the Darwin regression tests.
type darwinVerificationDescendantOwner struct{}

func newVerificationDescendantOwner() (*darwinVerificationDescendantOwner, error) {
	return &darwinVerificationDescendantOwner{}, nil
}

func (*darwinVerificationDescendantOwner) Close() error { return nil }

func (*darwinVerificationDescendantOwner) Terminate(targetPID int) error {
	killVerificationProcessGroup(targetPID)
	return nil
}
