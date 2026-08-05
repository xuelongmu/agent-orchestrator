//go:build !darwin

package launchagenthandoff

import (
	"context"
	"errors"
	"time"
)

func acquireLock(context.Context, string, time.Duration) (func(), error) {
	return nil, errors.New("launch-agent environment handoff is only available on macOS")
}

func prepareEnvironmentSocket(string, map[string]string) (<-chan error, func(), error) {
	return nil, nil, errors.New("launch-agent environment handoff is only available on macOS")
}

func waitForEnvironmentDelivery(context.Context, <-chan error, <-chan struct{}) error {
	return errors.New("launch-agent environment handoff is only available on macOS")
}
