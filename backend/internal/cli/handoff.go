package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

type handoffOptions struct {
	changedFiles         []string
	verificationCommands []string
	residualRisk         string
}

// handoffRequest mirrors SubmitSessionHandoffRequest without importing the
// daemon controller package across the deliberate CLI/API boundary.
type handoffRequest struct {
	ChangedFiles         []string `json:"changedFiles"`
	VerificationCommands []string `json:"verificationCommands"`
	ResidualRisk         string   `json:"residualRisk"`
}

type handoffResponse struct {
	SessionID string `json:"sessionId"`
	Created   bool   `json:"created"`
}

func newHandoffCommand(ctx *commandContext) *cobra.Command {
	var opts handoffOptions
	cmd := &cobra.Command{
		Use:   "handoff",
		Short: "Submit this agent session's structured completion handoff",
		Long: "Submit an immutable structured completion handoff for the current AO session.\n" +
			"Call this once when the task is completed or ready for review; the explicit\n" +
			"local API call seals the payload, while an exact replay is safe.\n" +
			"This records changed files, verification commands, and residual risk only;\n" +
			"it does not complete, terminate, or otherwise change session lifecycle state.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctx.submitHandoff(cmd.Context(), cmd, opts)
		},
	}
	cmd.Flags().StringArrayVar(&opts.changedFiles, "changed-file", nil, "Changed file path (repeatable)")
	cmd.Flags().StringArrayVar(&opts.verificationCommands, "verification-command", nil, "Verification command run (repeatable)")
	cmd.Flags().StringVar(&opts.residualRisk, "residual-risk", "", "Remaining risk or deferred verification")
	return cmd
}

func (c *commandContext) submitHandoff(ctx context.Context, cmd *cobra.Command, opts handoffOptions) error {
	sessionID := strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
	if sessionID == "" {
		return usageError{errors.New("ao handoff must run inside an AO session (AO_SESSION_ID is not set)")}
	}
	// Preserve flag order and exact strings. Use non-nil empty arrays so the
	// required typed API shape is explicit even for a no-files/no-tests handoff.
	changedFiles := append([]string{}, opts.changedFiles...)
	commands := append([]string{}, opts.verificationCommands...)
	var response handoffResponse
	path := "sessions/" + url.PathEscape(sessionID) + "/handoff"
	if err := c.postJSON(ctx, path, handoffRequest{ChangedFiles: changedFiles, VerificationCommands: commands, ResidualRisk: opts.residualRisk}, &response); err != nil {
		return err
	}
	if response.Created {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Handoff submitted for %s.\n", response.SessionID)
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "Handoff already recorded for %s (exact replay).\n", response.SessionID)
	return err
}
