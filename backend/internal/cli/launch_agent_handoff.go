package cli

import (
	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/launchagenthandoff"
)

func newLaunchAgentHandoffCommand() *cobra.Command {
	var lockPath string
	cmd := &cobra.Command{
		Use:    "launch-agent-handoff",
		Hidden: true,
		Args:   noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return launchagenthandoff.Run(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), lockPath)
		},
	}
	cmd.Flags().StringVar(&lockPath, "lock", "", "path to the exclusive handoff lock file")
	_ = cmd.MarkFlagRequired("lock")
	return cmd
}
