package cli

import (
	"errors"
	"fmt"

	"github.com/shinession/gophant/internal/meta"
	"github.com/shinession/gophant/internal/snapshot"
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
releases the WAL that s1 keeps for it. Switch the application to s3 before or right after.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := o.setup(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			src, dst, err := verify.OpenPools(ctx, env.cfg)
			if err != nil {
				return err
			}
			defer src.Close()
			defer dst.Close()

			store := meta.NewStore(env.cfg.Target.MetaSchema)
			state, err := store.GetState(ctx, dst)
			if err != nil {
				return err
			}
			if state == nil {
				return errors.New("no migration state found on s3: nothing to verify")
			}
			rehearsal := state.Stage != meta.StageVerify && state.Stage != meta.StageDone
			if rehearsal && !force {
				return fmt.Errorf("the migration is in stage %s: run the cutover first, or pass --force to compare anyway "+
					"(rows that s1 is still writing will show up as differences)", state.Stage)
			}
			if dropSlot && rehearsal {
				return fmt.Errorf("--drop-slot is only allowed in stage %s or %s, the migration is in stage %s", meta.StageVerify, meta.StageDone, state.Stage)
			}

			v := &verify.Verifier{Cfg: env.cfg, Src: src, Dst: dst, Logger: env.logger}
			report, err := v.Run(ctx, store)
			if err != nil {
				return err
			}
			report.Print(env.out)
			if report.Failed() {
				return errors.New("the verification found differences between s1 and s3")
			}
			if rehearsal {
				return nil
			}
			if !dropSlot {
				fmt.Fprintf(env.out, "\nSwitch the application to s3 now. Afterwards release the WAL that s1 keeps for the slot:\n"+
					"  gophant verify --drop-slot -c %s\n", o.configPath)
				return nil
			}
			if err := snapshot.DropSlot(ctx, src, env.cfg.Source.SlotName, env.logger); err != nil {
				return err
			}
			if err := store.SetStage(ctx, dst, meta.StageDone); err != nil {
				return err
			}
			fmt.Fprintf(env.out, "\nThe replication slot %s was dropped and the migration is DONE.\n", env.cfg.Source.SlotName)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dropSlot, "drop-slot", false, "drop the replication slot on s1 when the verification passed")
	cmd.Flags().BoolVar(&force, "force", false, "verify even though the migration has not reached the VERIFY stage (differences are expected while s1 is written)")
	return cmd
}
