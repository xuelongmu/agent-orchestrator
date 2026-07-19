package cli

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/spf13/cobra"
)

type contractAddRequest struct {
	PR        string `json:"pr"`
	Invariant string `json:"invariant"`
}

func newContractCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{Use: "contract", Short: "Manage durable per-PR design contracts"}
	var session, pr, invariant string
	add := &cobra.Command{
		Use:   "add",
		Short: "Append one explicit invariant to an owned PR contract",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id := strings.TrimSpace(session)
			if id == "" {
				id = strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
			}
			if id == "" || strings.TrimSpace(pr) == "" || strings.TrimSpace(invariant) == "" {
				return usageError{errors.New("usage: ao contract add --pr <url-or-number> --invariant <one-line-text> [--session <id>]")}
			}
			var response struct {
				OK bool `json:"ok"`
			}
			path := "sessions/" + url.PathEscape(id) + "/design-contract/invariants"
			if err := ctx.postJSON(cmd.Context(), path, contractAddRequest{PR: pr, Invariant: invariant}, &response); err != nil {
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "recorded invariant for %s\n", pr)
			return err
		},
	}
	add.Flags().StringVar(&session, "session", "", "Owning worker session id (defaults to AO_SESSION_ID)")
	add.Flags().StringVar(&pr, "pr", "", "Exact PR URL or number")
	add.Flags().StringVar(&invariant, "invariant", "", "Plain one-line invariant (max 512 bytes)")
	cmd.AddCommand(add)
	var showSession, showPR string
	show := &cobra.Command{
		Use:   "show",
		Short: "Print the full canonical contract for an owned PR",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id := strings.TrimSpace(showSession)
			if id == "" {
				id = strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
			}
			if id == "" || strings.TrimSpace(showPR) == "" {
				return usageError{errors.New("usage: ao contract show --pr <url-or-number> [--session <id>]")}
			}
			var response struct {
				Contract string `json:"contract"`
			}
			path := "sessions/" + url.PathEscape(id) + "/design-contract?pr=" + url.QueryEscape(showPR)
			if err := ctx.getJSON(cmd.Context(), path, &response); err != nil {
				return err
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), domain.SanitizeControlChars(response.Contract))
			return err
		},
	}
	show.Flags().StringVar(&showSession, "session", "", "Owning worker session id (defaults to AO_SESSION_ID)")
	show.Flags().StringVar(&showPR, "pr", "", "Exact PR URL or number")
	cmd.AddCommand(show)
	return cmd
}
