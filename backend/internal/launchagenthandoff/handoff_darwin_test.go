//go:build darwin

package launchagenthandoff

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunRestoresEnvironmentWhenElectronPipeCloses(t *testing.T) {
	originalRunLaunchctl := runLaunchctl
	t.Cleanup(func() { runLaunchctl = originalRunLaunchctl })
	state := map[string]string{"GH_TOKEN": "old-token"}
	runLaunchctl = func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "getenv":
			value, ok := state[args[1]]
			if !ok {
				return nil, errors.New("not set")
			}
			return []byte(value + "\n"), nil
		case "setenv":
			state[args[1]] = args[2]
		case "unsetenv":
			delete(state, args[1])
		}
		return nil, nil
	}

	input := strings.NewReader(`{"environment":{"GH_TOKEN":"new-token","OPENAI_API_KEY":"api-token"}}` + "\n")
	var output strings.Builder
	if err := Run(context.Background(), input, &output); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if output.String() != "ready\n" {
		t.Fatalf("output = %q, want ready", output.String())
	}
	if state["GH_TOKEN"] != "old-token" {
		t.Fatalf("GH_TOKEN = %q, want restored old value", state["GH_TOKEN"])
	}
	if _, ok := state["OPENAI_API_KEY"]; ok {
		t.Fatal("OPENAI_API_KEY remained after parent pipe closed")
	}
}
