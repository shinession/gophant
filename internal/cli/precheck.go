package cli

import (
	"errors"
	"fmt"

	"github.com/shinession/gophant/internal/precheck"
	"github.com/spf13/cobra"
)

// errNotImplemented marks a stage that a later milestone still has to deliver.
var errNotImplemented = errors.New("not implemented yet")

func newPrecheckCommand(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "precheck",
		Short: "Stage 1a: check the environment and compatibility of s1 and s3",
		Long: `Checks s1 and s3 and prints a PASS / WARN / FAIL report. The command only reads from both
databases. It exits with an error when at least one check fails.

With databases.auto the databases of s1 are found first: the checks of the cluster are shown once and
the checks of each database next to its name, with the statements that fix what gophant may not fix
itself (a publication on s1, a database on s3).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := o.setup(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			if env.cfg.Databases.Auto {
				fleet, err := o.openFleet(ctx, cmd, env, false, "gophant-precheck")
				if err != nil {
					return err
				}
				defer fleet.Close()
				report, err := fleet.Precheck(ctx, fleet.Databases, fleet.Databases)
				if err != nil {
					return err
				}
				report.Print(env.out)
				if report.Failed() {
					return fmt.Errorf("precheck failed: %d check(s) failed", report.FailureCount())
				}
				return nil
			}
			if err := o.requireAuto(env); err != nil {
				return err
			}

			p, err := openPools(ctx, env.cfg, "gophant-precheck")
			if err != nil {
				return err
			}
			defer p.Close()

			report, err := precheck.New(env.cfg, p.src, p.dst, env.logger).Run(ctx)
			if err != nil {
				return err
			}
			report.Print(env.out)
			if report.Failed() {
				_, _, failed := report.Counts()
				return fmt.Errorf("precheck failed: %d check(s) failed", failed)
			}
			return nil
		},
	}
}
