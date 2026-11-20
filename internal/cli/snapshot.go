package cli

import (
	"github.com/shinession/gophant/internal/snapshot"
	"github.com/spf13/cobra"
)

func newSnapshotCommand(o *options) *cobra.Command {
	var restart, yes bool
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Stage 1b and 2: import pre-data, create the slot, copy all tables, import post-data",
		Long: `Runs the precheck, imports the table definitions (pre-data) on s3, creates the replication slot with
an exported snapshot, copies every table in parallel with streaming COPY, and finally imports the
indexes and constraints (post-data). The migration then moves to the CDC_STREAM stage.

The exported snapshot only lives while this command runs, so an interrupted copy cannot be resumed
table by table. Use --restart to discard it (the slot is dropped, the copied rows are truncated)
and start over. A copy that finished but whose post-data import failed is continued by running the
command again.

With databases.auto this is done for every selected database, databases.parallel at a time, after
one precheck of all of them; a database whose snapshot is complete is left alone. Use 'gophant run'
to also start the streams while the later databases are still being copied.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := o.setup(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			if env.cfg.Databases.Auto {
				fleet, err := o.openFleet(ctx, cmd, env, yes, "gophant-snapshot")
				if err != nil {
					return err
				}
				defer fleet.Close()
				return fleet.Snapshot(ctx, restart)
			}
			if err := o.requireAuto(env); err != nil {
				return err
			}

			p, err := openPools(ctx, env.cfg, "gophant-snapshot")
			if err != nil {
				return err
			}
			defer p.Close()

			runner := &snapshot.Runner{
				Cfg: env.cfg, Src: p.src, Dst: p.dst, Logger: env.logger, Out: env.out,
				Confirm: confirmer(cmd.InOrStdin(), env.out, yes),
			}
			return runner.Run(ctx, snapshot.Options{Restart: restart})
		},
	}
	cmd.Flags().BoolVar(&restart, "restart", false, "discard an existing snapshot (drop the slot, truncate the copied tables) and start over")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation before destructive steps")
	return cmd
}
