//go:build darwin

package launchagenthandoff

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

var runLaunchctl = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "/bin/launchctl", args...).Output()
}

var environmentKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func launchdEnvironmentKeys(ctx context.Context) ([]string, error) {
	output, err := runLaunchctl(ctx, "print", fmt.Sprintf("gui/%d", os.Getuid()))
	if err != nil {
		return nil, fmt.Errorf("inspect launchd environment: %w", err)
	}
	keys := map[string]struct{}{}
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	inEnvironment := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasSuffix(line, "environment = {") {
			inEnvironment = true
			continue
		}
		if !inEnvironment {
			continue
		}
		if line == "}" {
			inEnvironment = false
			continue
		}
		key, _, found := strings.Cut(line, "=>")
		key = strings.TrimSpace(key)
		if found && environmentKeyPattern.MatchString(key) {
			keys[key] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parse launchd environment: %w", err)
	}
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sort.Strings(result)
	return result, nil
}

func install(ctx context.Context, environment map[string]string) (func() error, error) {
	ambientKeys, err := launchdEnvironmentKeys(ctx)
	if err != nil {
		return nil, err
	}
	keySet := make(map[string]struct{}, len(ambientKeys)+len(environment))
	ambientKeySet := make(map[string]struct{}, len(ambientKeys))
	for _, key := range ambientKeys {
		keySet[key] = struct{}{}
		ambientKeySet[key] = struct{}{}
	}
	for key := range environment {
		keySet[key] = struct{}{}
	}
	keys := make([]string, 0, len(keySet))
	for key := range keySet {
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
		item := previousValue{key: key}
		if _, present := ambientKeySet[key]; present {
			output, err := runLaunchctl(ctx, "getenv", key)
			if err != nil {
				_ = restore()
				return nil, fmt.Errorf("snapshot launchd environment %s: %w", key, err)
			}
			item.present = true
			item.value = strings.TrimSuffix(string(output), "\n")
		}
		previous = append(previous, item)
		value, desired := environment[key]
		action := "unsetenv"
		args := []string{action, key}
		if desired {
			action = "setenv"
			args = []string{action, key, value}
		}
		if _, err := runLaunchctl(ctx, args...); err != nil {
			_ = restore()
			return nil, fmt.Errorf("%s launchd environment %s: %w", action, key, err)
		}
	}
	return restore, nil
}
