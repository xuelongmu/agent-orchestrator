package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// Build metadata. Release tooling can override these with -ldflags.
var (
	Version    = "dev"
	Commit     = ""
	Date       = ""
	Repository = ""
	Channel    = ""
)

// BuildCapabilities names behavior that callers should detect directly instead
// of inferring it from semver or PR history. Keep identifiers append-only once
// released so scripts can safely test membership.
var BuildCapabilities = []string{
	"codex-primary-worktree-hooks-v1",
	"project-environment-patch-v1",
}

// BuildInfo is the machine-readable `ao version --json` response.
type BuildInfo struct {
	Version      string   `json:"version"`
	Commit       string   `json:"commit,omitempty"`
	BuiltAt      string   `json:"builtAt,omitempty"`
	Repository   string   `json:"repository,omitempty"`
	Channel      string   `json:"channel,omitempty"`
	Capabilities []string `json:"capabilities"`
}

func currentBuildInfo() BuildInfo {
	return BuildInfo{
		Version:      Version,
		Commit:       Commit,
		BuiltAt:      Date,
		Repository:   Repository,
		Channel:      Channel,
		Capabilities: append([]string(nil), BuildCapabilities...),
	}
}

// VersionString renders the build metadata as "<version> commit <c> built <d>",
// omitting the commit/date parts when they are unset.
func VersionString() string {
	parts := []string{Version}
	if Commit != "" {
		parts = append(parts, "commit "+Commit)
	}
	if Date != "" {
		parts = append(parts, "built "+Date)
	}
	return strings.Join(parts, " ")
}

func newVersionCommand() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), currentBuildInfo())
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), VersionString())
			return err
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output build provenance and capabilities as JSON")
	return cmd
}
