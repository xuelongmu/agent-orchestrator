//go:build !darwin

package launchagenthandoff

import (
	"context"
	"errors"
)

func prepareEnvironmentSocket(string, map[string]string) (<-chan error, func(), error) {
	return nil, nil, errors.New("launch-agent environment handoff is only available on macOS")
}

func waitForEnvironmentDelivery(context.Context, <-chan error, <-chan struct{}) error {
	return errors.New("launch-agent environment handoff is only available on macOS")
}
