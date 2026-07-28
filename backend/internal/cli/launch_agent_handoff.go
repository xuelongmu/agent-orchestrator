package cli

import (
	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/launchagenthandoff"
)

func newLaunchAgentHandoffCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "launch-agent-handoff",
		Hidden: true,
		Args:   noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return launchagenthandoff.Run(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}
