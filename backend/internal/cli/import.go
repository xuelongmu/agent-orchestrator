package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/legacyimport"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

type importOptions struct {
	from   string
	dryRun bool
	yes    bool
	json   bool
}

func newImportCommand(ctx *commandContext) *cobra.Command {
	var opts importOptions
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import projects from a legacy AO install",
		Long: "Import reads the legacy Agent Orchestrator flat-file store " +
			"(~/.agent-orchestrator) read-only and ports its projects and per-project " +
			"settings into the rewrite database. Legacy files are never modified, and " +
			"a re-run skips rows that already exist, so it is safe to run more than once.\n\n" +
			"The daemon must be stopped: it is the sole writer of the database. Dry-run " +
			"reconciliation uses a private copy of ao.db and its WAL instead of opening " +
			"the source. The snapshot is consistent while those files are quiescent, so " +
			"other standalone database writers must also be stopped while it is taken.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.runImport(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.from, "from", "", "Legacy AO root to read (default ~/.agent-orchestrator)")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Parse and report the planned import without writing")
	cmd.Flags().BoolVarP(&opts.yes, "yes", "y", false, "Skip the confirmation prompt (for non-interactive use)")
	cmd.Flags().BoolVar(&opts.json, "json", false, "Output the import report as JSON")
	return cmd
}

func (c *commandContext) runImport(cmd *cobra.Command, opts importOptions) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// The daemon is the sole writer; refuse to open the store underneath a live
	// one. A stale run-file (dead PID) is treated as safe.
	if live, err := runfile.CheckStale(cfg.RunFilePath); err != nil {
		return fmt.Errorf("inspect run-file: %w", err)
	} else if live != nil {
		return usageError{fmt.Errorf("the AO daemon is running (pid %d); stop it first with `ao stop` before importing", live.PID)}
	}

	root := opts.from
	if root == "" {
		root = legacyimport.DefaultLegacyRootDir()
	}
	// Surface a parse error instead of masking it as "no data" (issue #2186):
	// a broken legacy store is distinct from an absent or empty one. Return the
	// error so cmd/ao/main.go renders it once; printing here too would duplicate
	// it on stderr.
	if parseErr := legacyimport.LegacyConfigError(cmd.Context(), root); parseErr != nil {
		return fmt.Errorf("legacy config at %s: %w", root, parseErr)
	}
	if !legacyimport.HasLegacyData(root) {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "No legacy AO projects found at %s. Nothing to import.\n", root)
		return err
	}

	if !opts.dryRun && !opts.yes {
		ok, err := confirm(c.deps.In, cmd.OutOrStdout(),
			fmt.Sprintf("Import projects from %s?", root), true)
		if err != nil {
			return err
		}
		if !ok {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "Import cancelled.")
			return err
		}
	}

	rep, err := c.executeImport(cmd.Context(), cfg, legacyimport.Options{
		Root:   root,
		DryRun: opts.dryRun,
	})
	if err != nil {
		return err
	}

	if opts.json {
		return writeJSON(cmd.OutOrStdout(), rep)
	}
	return writeImportSummary(cmd.OutOrStdout(), rep)
}

// executeImport opens the rewrite store, runs the import, and closes the store.
// It is the one CLI path that opens the database directly: the import is a
// one-time bootstrap that cannot go through the daemon's loopback API. A
// non-dry-run import takes the same OS + renewable exclusive-writer lease as
// the daemon before opening or migrating SQLite, closing the
// confirmation-prompt/startup TOCTOU left by the preliminary run-file check.
func (c *commandContext) executeImport(ctx context.Context, cfg config.Config, opts legacyimport.Options) (rep legacyimport.Report, err error) {
	if opts.DryRun {
		store, cleanup, openErr := c.openImportPlanningStore(cfg.DataDir)
		if openErr != nil {
			return legacyimport.Report{}, fmt.Errorf("open planning store: %w", openErr)
		}
		defer func() {
			if cleanupErr := cleanup(); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("close planning store: %w", cleanupErr))
			}
		}()
		return legacyimport.Run(ctx, store, opts)
	}

	store, lease, err := c.deps.OpenExclusiveStore(ctx, cfg.DataDir, os.Getpid())
	if err != nil {
		return legacyimport.Report{}, fmt.Errorf("open exclusive import store: %w", err)
	}
	defer func() { _ = store.Close() }()
	defer func() {
		if releaseErr := lease.Release(context.Background()); releaseErr != nil && err == nil {
			err = fmt.Errorf("release import database-writer lease: %w", releaseErr)
		}
	}()
	writeCtx, cancel := context.WithCancel(ctx)
	leaseDone := lease.Maintain(writeCtx, cancel, nil)
	defer func() {
		cancel()
		<-leaseDone
	}()
	return legacyimport.Run(writeCtx, store, opts)
}

// openImportPlanningStore returns a source-independent view for a dry run.
// sqlite.Open cannot be used against the source even with mode=ro: on Windows
// SQLite may still create a zero-byte WAL beside a read-only database. For an
// existing, quiescent database, copy the database and WAL as a pair and let
// SQLite rebuild private SHM state, run migrations, and query only that copy.
// runImport's live-daemon exclusion supplies the normal quiescence boundary;
// the command help calls out that other standalone writers must also be idle.
func (c *commandContext) openImportPlanningStore(dataDir string) (legacyimport.Store, func() error, error) {
	sourceDB := filepath.Join(dataDir, "ao.db")
	if _, err := os.Stat(sourceDB); errors.Is(err, os.ErrNotExist) {
		return emptyImportPlanningStore{}, func() error { return nil }, nil
	} else if err != nil {
		return nil, nil, fmt.Errorf("inspect source database: %w", err)
	}

	snapshotDir, err := os.MkdirTemp("", "ao-import-plan-")
	if err != nil {
		return nil, nil, fmt.Errorf("create private database snapshot directory: %w", err)
	}
	removeSnapshot := func() error { return c.deps.RemoveAll(snapshotDir) }

	for _, name := range []string{"ao.db", "ao.db-wal"} {
		err := copyImportSnapshotFile(filepath.Join(dataDir, name), filepath.Join(snapshotDir, name))
		if name == "ao.db-wal" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, errors.Join(fmt.Errorf("copy source %s: %w", name, err), removeSnapshot())
		}
	}

	store, err := c.deps.OpenStore(snapshotDir)
	if err != nil {
		return nil, nil, errors.Join(err, removeSnapshot())
	}
	cleanup := func() error {
		closeErr := store.Close()
		removeErr := removeSnapshot()
		return errors.Join(closeErr, removeErr)
	}
	return store, cleanup, nil
}

func copyImportSnapshotFile(source, destination string) (retErr error) {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, in.Close()) }()

	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, out.Close()) }()
	_, err = io.Copy(out, in)
	return err
}

// emptyImportPlanningStore models a missing rewrite database without creating
// the source data directory. Dry-run never calls UpsertProject; keep the method
// fail-closed so this planning-only store cannot silently become writable.
type emptyImportPlanningStore struct{}

func (emptyImportPlanningStore) GetProject(context.Context, string) (domain.ProjectRecord, bool, error) {
	return domain.ProjectRecord{}, false, nil
}

func (emptyImportPlanningStore) UpsertProject(context.Context, domain.ProjectRecord) error {
	return errors.New("empty import planning store is read-only")
}

func writeImportSummary(w io.Writer, rep legacyimport.Report) error {
	var b strings.Builder
	if rep.DryRun {
		b.WriteString("Dry run -- no changes written.\n")
	}
	fmt.Fprintf(&b, "Projects:  %d imported, %d already present\n", rep.ProjectsImported, rep.ProjectsSkipped)
	if len(rep.Notes) > 0 {
		b.WriteString("\nNotes:\n")
		for _, n := range rep.Notes {
			fmt.Fprintf(&b, "  - %s\n", n)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// confirm prompts for a yes/no answer. When stdin is not an interactive
// terminal it returns the default without prompting, so headless invocations
// behave deterministically.
func confirm(in io.Reader, out io.Writer, prompt string, defaultYes bool) (bool, error) {
	suffix := " [Y/n] "
	if !defaultYes {
		suffix = " [y/N] "
	}
	if !stdinIsInteractive(in) {
		return defaultYes, nil
	}
	if _, err := io.WriteString(out, prompt+suffix); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		// EOF with no input: fall back to the default.
		return defaultYes, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return defaultYes, nil
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return false, nil
	}
}

// stdinIsInteractive reports whether in is an interactive terminal. It only
// treats the real os.Stdin as potentially interactive; a piped reader or test
// buffer is non-interactive.
func stdinIsInteractive(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
