package cli

import (
	"fmt"

	"github.com/shinession/gophant/internal/verify"
	"github.com/spf13/cobra"
)

func newVerifyCommand(o *options) *cobra.Command {
	var dropSlot, force bool
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Stage 5: compare row counts and primary-key chunk hashes of s1 and s3",
		Long: `Compares s1 and s3 table by table: the exact row count first, then (depending on verify.mode)
fingerprints of primary key ranges, all of them or a sample. Run it after the cutover, while s1 is
still write-stopped; during a live migration differences are expected.

With --drop-slot the replication slot of s1 is dropped after a successful verification, which
releases the WAL that s1 keeps for it. Switch the application to s3 before or right after.

With databases.auto every selected database is verified (--database limits the list), one after the
other, or databases.parallel at a time.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := o.setup(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			opts := verify.StageOptions{DropSlot: dropSlot, Force: force}

			if env.cfg.Databases.Auto {
				fleet, err := o.openFleet(ctx, cmd, env, false, "gophant-verify")
				if err != nil {
					return err
				}
				defer fleet.Close()
				return fleet.Verify(ctx, opts, o.configPath)
			}
			if err := o.requireAuto(env); err != nil {
				return err
			}

			res, err := verify.RunStage(ctx, env.cfg, env.logger, env.out, opts)
			if err != nil {
				return err
			}
			if !res.Rehearsal && !res.Dropped {
				fmt.Fprintf(env.out, "\nSwitch the application to s3 now. Afterwards release the WAL that s1 keeps for the slot:\n"+
					"  gophant verify --drop-slot -c %s\n", o.configPath)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dropSlot, "drop-slot", false, "drop the replication slot on s1 when the verification passed")
	cmd.Flags().BoolVar(&force, "force", false, "verify even though the migration has not reached the VERIFY stage (differences are expected while s1 is written)")
	return cmd
}
