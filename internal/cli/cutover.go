package cli

import (
	"github.com/shinession/gophant/internal/cutover"
	"github.com/spf13/cobra"
)

func newCutoverCommand(o *options) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "cutover",
		Short: "Stage 4: wait for lag 0, synchronize all sequences, move to verification",
		Long: `Runs inside the downtime window. Stop every write to s1 first (stop the application or set the
database read-only); gophant does not do that for you. The command then waits until s3 has applied
everything s1 wrote (lag 0, with no open write transaction on s1), copies the position of every
sequence to s3 (setval with a safety offset) and moves the migration to the VERIFY stage.

gophant sync must be running: it applies the last transactions and stops by itself afterwards.

With databases.auto this is done for all selected databases at the same time, after one question.
It refuses to start while the snapshot of a selected database is not finished; --database limits it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := o.setup(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			if env.cfg.Databases.Auto {
				fleet, err := o.openFleet(ctx, cmd, env, yes, "gophant-cutover")
				if err != nil {
					return err
				}
				defer fleet.Close()
				return fleet.Cutover(ctx)
			}
			if err := o.requireAuto(env); err != nil {
				return err
			}

			p, err := openPools(ctx, env.cfg, "gophant-cutover")
			if err != nil {
				return err
			}
			defer p.Close()

			r := &cutover.Runner{
				Cfg: env.cfg, Src: p.src, Dst: p.dst, Logger: env.logger, Out: env.out,
				Confirm: confirmer(cmd.InOrStdin(), env.out, yes),
			}
			return r.Run(ctx)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm without asking that all writes to s1 have been stopped")
	return cmd
}
