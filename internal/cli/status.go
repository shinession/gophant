package cli

import (
	"github.com/shinession/gophant/internal/pg"
	"github.com/shinession/gophant/internal/status"
	"github.com/spf13/cobra"
)

func newStatusCommand(o *options) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the stage, the LSN lag in bytes and the copy progress of every table",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := o.setup(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			dst, err := pg.NewPool(ctx, env.cfg.Target.DSN, "gophant-status", 2)
			if err != nil {
				return err
			}
			defer dst.Close()
			// s1 is optional here: the status of s3 is still useful when s1 cannot be reached.
			src, srcErr := pg.NewPool(ctx, env.cfg.Source.DSN, "gophant-status", 2)
			if srcErr != nil {
				env.logger.Warn("s1 is not reachable, showing what s3 knows", "error", srcErr)
			} else {
				defer src.Close()
			}

			info, err := status.Collect(ctx, env.cfg, src, dst)
			if err != nil {
				return err
			}
			if srcErr != nil {
				info.SourceError = srcErr.Error()
			}
			if asJSON {
				return info.WriteJSON(env.out)
			}
			info.Print(env.out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the status as JSON")
	return cmd
}
