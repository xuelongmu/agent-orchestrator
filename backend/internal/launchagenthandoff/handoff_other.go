//go:build !darwin

package launchagenthandoff

import (
	"context"
	"errors"
)

func install(context.Context, map[string]string) (func() error, error) {
	return nil, errors.New("launch-agent environment handoff is only available on macOS")
}
