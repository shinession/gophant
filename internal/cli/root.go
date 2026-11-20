// Package cli defines the gophant command line: one cobra command per migration stage.
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/shinession/gophant/internal/config"
	"github.com/spf13/cobra"
)

// options holds the persistent flags shared by every subcommand.
type options struct {
	configPath string
	logLevel   string
	logFormat  string
	// databases limits the multi-database mode to the named databases.
	databases []string
}

// Execute runs the gophant command line and returns the process exit code.
func Execute(version string) int {
	// SIGINT/SIGTERM cancel the context; each stage then drains and exits gracefully.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := NewRootCommand(version, os.Stdout, os.Stderr)
	root.SetContext(ctx)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// NewRootCommand builds the command tree. The writers are injectable for tests.
func NewRootCommand(version string, stdout, stderr io.Writer) *cobra.Command {
	opts := &options{}
	root := &cobra.Command{
		Use:   "gophant",
		Short: "PostgreSQL 16 to PostgreSQL 18 incremental migration without data loss",
		Long: `gophant migrates a PostgreSQL 16 database (s1) to PostgreSQL 18 (s3) with a short cutover window.

It copies a consistent snapshot with parallel binary COPY, then replays the logical WAL stream
(pgoutput) exactly once until the cutover, and finally verifies the result.

Stages: precheck -> snapshot -> sync -> cutover -> verify (status shows the progress at any time).

With databases.auto in the configuration every command works on all the databases of s1 instead of
one: they are found automatically, and "run" does the precheck, the snapshot and the sync of all of
them in one go. --database limits a command to some of them.`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.PersistentFlags().StringVarP(&opts.configPath, "config", "c", "gophant.yaml", "path of the YAML configuration file")
	root.PersistentFlags().StringVar(&opts.logLevel, "log-level", "info", "log level: debug, info, warn or error")
	root.PersistentFlags().StringVar(&opts.logFormat, "log-format", "json", "log format: json or text")
	root.PersistentFlags().StringArrayVarP(&opts.databases, "database", "d", nil,
		"with databases.auto: work on this database only (repeat the flag for several)")

	root.AddCommand(
		newPrecheckCommand(opts),
		newSnapshotCommand(opts),
		newSyncCommand(opts),
		newCutoverCommand(opts),
		newVerifyCommand(opts),
		newStatusCommand(opts),
		newRunCommand(opts),
	)
	return root
}

// runtimeEnv is what every stage needs: the validated configuration and a logger.
type runtimeEnv struct {
	cfg    *config.Config
	logger *slog.Logger
	out    io.Writer
}

// setup loads the configuration and builds the logger. Logs go to stderr so that the reports that
// commands print on stdout stay clean.
func (o *options) setup(cmd *cobra.Command) (*runtimeEnv, error) {
	logger, err := newLogger(cmd.ErrOrStderr(), o.logLevel, o.logFormat)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(o.configPath)
	if err != nil {
		return nil, err
	}
	return &runtimeEnv{cfg: cfg, logger: logger, out: cmd.OutOrStdout()}, nil
}

func newLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid --log-level %q: use debug, info, warn or error", level)
	}
	handlerOpts := &slog.HandlerOptions{Level: lvl}
	switch strings.ToLower(format) {
	case "json":
		return slog.New(slog.NewJSONHandler(w, handlerOpts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, handlerOpts)), nil
	default:
		return nil, fmt.Errorf("invalid --log-format %q: use json or text", format)
	}
}
