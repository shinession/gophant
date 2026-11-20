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
databases. It exits with an error when at least one check fails.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := o.setup(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
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
