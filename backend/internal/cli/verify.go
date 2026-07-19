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

type verifyAPIRequest struct {
	Profile string `json:"profile"`
}

type verifyAPIResponse struct {
	SessionID  string `json:"sessionId"`
	Profile    string `json:"profile"`
	Outcome    string `json:"outcome"`
	ExitCode   int    `json:"exitCode"`
	LogPath    string `json:"logPath"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"durationMs"`
	Error      string `json:"error,omitempty"`
}

func newVerifyCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:     "verify <profile>",
		Short:   "Run an allowed workspace verification outside the agent terminal",
		Example: "  ao verify backend\n  ao verify frontend",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.ExactArgs(1)(cmd, args); err != nil {
				return usageError{err}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.verify(cmd.Context(), args[0])
		},
	}
}

func (c *commandContext) verify(ctx context.Context, profile string) error {
	sessionID := strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
	if sessionID == "" {
		return usageError{errors.New("ao verify must run inside an AO session (AO_SESSION_ID is not set)")}
	}
	var result verifyAPIResponse
	path := "sessions/" + url.PathEscape(sessionID) + "/verify"
	if err := c.postLongJSON(ctx, path, verifyAPIRequest{Profile: profile}, &result); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.deps.Out, "outcome: %s\nlog: %s\n", result.Outcome, result.LogPath)
	if result.Truncated {
		_, _ = fmt.Fprintln(c.deps.Out, "log truncated: true")
	}
	if result.Outcome != "passed" {
		message := fmt.Sprintf("verification %s (exit %d); see %s", result.Outcome, result.ExitCode, result.LogPath)
		if result.Error != "" {
			message += ": " + result.Error
		}
		return errors.New(message)
	}
	return nil
}
