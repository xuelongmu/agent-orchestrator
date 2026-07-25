//go:build !windows

package process

import (
	"context"
)

// ErrSessionJobNotFound is only non-nil on Windows. Other platforms use their
// native process-group/runtime teardown and treat session jobs as no-ops.
var ErrSessionJobNotFound error

// SessionJob is a no-op process owner on non-Windows platforms.
type SessionJob struct{}

// CreateSessionJob returns a no-op owner on non-Windows platforms.
func CreateSessionJob(_, _, _ string) (*SessionJob, error) {
	return &SessionJob{}, nil
}

// OpenSessionJob returns a no-op owner on non-Windows platforms.
func OpenSessionJob(_, _, _ string) (*SessionJob, error) {
	return &SessionJob{}, nil
}

// Assign is a no-op on non-Windows platforms.
func (j *SessionJob) Assign(_ int) error {
	return nil
}

// TerminateAndWait is a no-op on non-Windows platforms.
func (j *SessionJob) TerminateAndWait(_ context.Context) error {
	return nil
}

// Close is a no-op on non-Windows platforms.
func (j *SessionJob) Close() error {
	return nil
}
