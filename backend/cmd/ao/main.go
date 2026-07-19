package main

import (
	"fmt"
	"os"

	"github.com/aoagents/agent-orchestrator/backend/internal/cli"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/verification"
)

func main() {
	if code, handled := verification.RunHostFromEnvironment(); handled {
		os.Exit(code)
	}
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(cli.ExitCode(err))
	}
}
