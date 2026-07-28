//go:build darwin

package launchagenthandoff

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

var runLaunchctl = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "/bin/launchctl", args...).Output()
}

func install(ctx context.Context, environment map[string]string) (func() error, error) {
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	type previousValue struct {
		key     string
		value   string
		present bool
	}
	previous := make([]previousValue, 0, len(keys))
	restore := func() error {
		var firstErr error
		for i := len(previous) - 1; i >= 0; i-- {
			item := previous[i]
			var err error
			if item.present {
				_, err = runLaunchctl(context.Background(), "setenv", item.key, item.value)
			} else {
				_, err = runLaunchctl(context.Background(), "unsetenv", item.key)
			}
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("restore launchd environment %s: %w", item.key, err)
			}
		}
		return firstErr
	}

	for _, key := range keys {
		output, err := runLaunchctl(ctx, "getenv", key)
		item := previousValue{key: key}
		if err == nil {
			item.present = true
			item.value = strings.TrimSuffix(string(output), "\n")
		}
		if _, err := runLaunchctl(ctx, "setenv", key, environment[key]); err != nil {
			_ = restore()
			return nil, fmt.Errorf("set launchd environment %s: %w", key, err)
		}
		previous = append(previous, item)
	}
	return restore, nil
}
