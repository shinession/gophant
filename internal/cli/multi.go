package cli

import (
	"context"
	"errors"

	"github.com/shinession/gophant/internal/multidb"
	"github.com/spf13/cobra"
)

// openFleet connects to s1 and s3 and finds the databases of the multi-database mode. assumeYes is the
// --yes flag of the command: it answers the questions that the command asks.
func (o *options) openFleet(ctx context.Context, cmd *cobra.Command, env *runtimeEnv, assumeYes bool, appName string) (*multidb.Fleet, error) {
	return multidb.Open(ctx, env.cfg, env.logger, env.out, confirmer(cmd.InOrStdin(), env.out, assumeYes), o.databases, appName)
}

// requireAuto rejects --database when the multi-database mode is off, where it would do nothing.
func (o *options) requireAuto(env *runtimeEnv) error {
	if len(o.databases) > 0 && !env.cfg.Databases.Auto {
		return errors.New("--database needs databases.auto: true in the configuration")
	}
	return nil
}

func newRunCommand(o *options) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "All databases: precheck, snapshot and sync in one command (needs databases.auto)",
		Long: `Finds the databases of s1, checks all of them, and for every database that passed creates what is
missing (the database on s3, the publication on s1), copies a consistent snapshot and replays the WAL
stream. The snapshots run databases.parallel at a time; the stream of a database starts as soon as its
own snapshot is done, while the next databases are still being copied, so s1 never keeps WAL for a
slot longer than one database takes to copy.

The command keeps running and replays the streams. Stop all writes to s1 and run 'gophant cutover'
in another terminal when the lag is zero; the streams then end by themselves and so does this command.
Run it again after an interruption: databases that stream already are not copied again.

A database whose checks failed is skipped after you agreed (--yes agrees); the others go on.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := o.setup(cmd)
			if err != nil {
				return err
			}
			if !env.cfg.Databases.Auto {
				return errors.New("gophant run works on all databases and needs databases.auto: true in the configuration; " +
					"for one database use 'gophant snapshot' and 'gophant sync'")
			}
			ctx := cmd.Context()
			fleet, err := o.openFleet(ctx, cmd, env, yes, "gophant-run")
			if err != nil {
				return err
			}
			defer fleet.Close()
			return fleet.Run(ctx)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the databases whose checks failed without asking")
	return cmd
}
