package cli

import (
	"github.com/shinession/gophant/internal/syncer"
	"github.com/spf13/cobra"
)

func newSyncCommand(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Stage 3: consume the WAL stream and replay it on s3 until stopped",
		Long: `Consumes the logical replication stream from the position stored on s3 (applied_lsn, or the
consistent point of the snapshot) and replays every committed transaction on s3. The business rows
and the new applied_lsn are committed in one s3 transaction, so the replay is exactly-once and can
be stopped and restarted at any time. A lost connection is resumed automatically.

Stop it with Ctrl-C or SIGTERM: the batch in progress is committed first. The command also ends by
itself once the cutover has completed.

With databases.auto the streams of all selected databases whose snapshot is done run at the same
time in this process; a database that is not ready is reported and left out, and a stream that
fails permanently stops alone.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := o.setup(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			if env.cfg.Databases.Auto {
				fleet, err := o.openFleet(ctx, cmd, env, false, "gophant-sync")
				if err != nil {
					return err
				}
				defer fleet.Close()
				return fleet.Sync(ctx)
			}
			if err := o.requireAuto(env); err != nil {
				return err
			}

			p, err := openPools(ctx, env.cfg, "gophant-sync")
			if err != nil {
				return err
			}
			defer p.Close()

			s := &syncer.Syncer{Cfg: env.cfg, Src: p.src, Dst: p.dst, Logger: env.logger, Out: env.out}
			return s.Run(ctx)
		},
	}
}
