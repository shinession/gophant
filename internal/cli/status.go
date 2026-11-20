package cli

import (
	"fmt"

	"github.com/shinession/gophant/internal/pg"
	"github.com/shinession/gophant/internal/status"
	"github.com/spf13/cobra"
)

func newStatusCommand(o *options) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the stage, the LSN lag in bytes and the copy progress of every table",
		Long: `Shows the stage, the lag, the replication slot and the progress of every table.

With databases.auto one line per database is shown, found on s1: s1 has to be reachable. Use
--database to see the tables of a single database.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := o.setup(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			if env.cfg.Databases.Auto {
				return o.multiStatus(cmd, env, asJSON)
			}
			if err := o.requireAuto(env); err != nil {
				return err
			}

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

// multiStatus prints one line per database, or the full status of the database that --database names.
func (o *options) multiStatus(cmd *cobra.Command, env *runtimeEnv, asJSON bool) error {
	ctx := cmd.Context()
	fleet, err := o.openFleet(ctx, cmd, env, false, "gophant-status")
	if err != nil {
		return err
	}
	defer fleet.Close()
	rep, err := fleet.Status(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		return rep.WriteJSON(env.out)
	}
	rep.Print(env.out)
	if len(rep.Databases) == 1 && rep.Databases[0].Info != nil {
		fmt.Fprintln(env.out)
		rep.Databases[0].Info.Print(env.out)
	}
	return nil
}
