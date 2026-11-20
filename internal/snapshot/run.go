package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shinession/gophant/internal/catalog"
	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/format"
	"github.com/shinession/gophant/internal/meta"
	"github.com/shinession/gophant/internal/pg"
	"github.com/shinession/gophant/internal/precheck"
	"github.com/shinession/gophant/internal/schema"
)

// Options are the switches of the snapshot command.
type Options struct {
	// Restart discards an unfinished or finished snapshot and starts over.
	Restart bool
}

// Runner orchestrates the snapshot stage: precheck, schema import, slot creation, the parallel copy
// and the post-data import.
type Runner struct {
	Cfg    *config.Config
	Src    *pgxpool.Pool
	Dst    *pgxpool.Pool
	Logger *slog.Logger
	// Out receives the human readable report.
	Out io.Writer
	// Confirm is asked before a destructive step. It returns false to cancel.
	Confirm func(prompt string) (bool, error)
}

type mode int

const (
	modeFresh mode = iota
	modeRestart
	// modeFinish continues a snapshot whose tables were all copied but whose post-data import failed.
	modeFinish
)

// Run executes the snapshot stage.
func (r *Runner) Run(ctx context.Context, opts Options) error {
	store := meta.NewStore(r.Cfg.Target.MetaSchema)
	state, err := store.GetState(ctx, r.Dst)
	if err != nil {
		return err
	}
	m, err := r.chooseMode(ctx, store, state, opts)
	if err != nil {
		return err
	}

	report, err := precheck.New(r.Cfg, r.Src, r.Dst, r.Logger).Run(ctx)
	if err != nil {
		return fmt.Errorf("precheck: %w", err)
	}
	report.Print(r.Out)
	if report.Failed() {
		return errors.New("the precheck failed: fix the failures above before the snapshot can start")
	}

	switch m {
	case modeRestart:
		ok, err := r.confirmRestart(ctx, store)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("restart cancelled")
		}
		if err := r.discard(ctx, store, state); err != nil {
			return err
		}
		return r.runFresh(ctx, store, true)
	case modeFinish:
		return r.finishPostData(ctx, store, state)
	default:
		return r.runFresh(ctx, store, false)
	}
}

// chooseMode decides between a fresh start, a restart and the continuation of a half finished run.
func (r *Runner) chooseMode(ctx context.Context, store *meta.Store, state *meta.SyncState, opts Options) (mode, error) {
	if state == nil {
		return modeFresh, nil // with --restart there is nothing to discard
	}
	if opts.Restart {
		return modeRestart, nil
	}
	switch state.Stage {
	case meta.StagePrecheck, meta.StageSnapshot:
		tables, err := store.ListTables(ctx, r.Dst)
		if err != nil {
			return 0, err
		}
		if len(tables) > 0 && state.ConsistentLSN != nil && allDone(tables) {
			return modeFinish, nil
		}
		return 0, fmt.Errorf("an unfinished snapshot exists (stage %s, %d of %d tables copied): "+
			"run snapshot --restart to discard it and start over", state.Stage, countDone(tables), len(tables))
	default:
		return 0, fmt.Errorf("the snapshot is already complete (stage %s): to discard the migration and start over run snapshot --restart", state.Stage)
	}
}

func allDone(tables []meta.TableProgress) bool { return countDone(tables) == len(tables) }

func countDone(tables []meta.TableProgress) int {
	n := 0
	for _, t := range tables {
		if t.SnapshotStatus == meta.StatusDone {
			n++
		}
	}
	return n
}

// runFresh runs stages 1b and 2 from the beginning. After a restart the tables and, maybe, the
// post-data objects of the previous attempt are still on s3.
func (r *Runner) runFresh(ctx context.Context, store *meta.Store, restarted bool) error {
	started := time.Now()
	if err := store.Ensure(ctx, r.Dst); err != nil {
		return err
	}
	if err := store.InitState(ctx, r.Dst, r.Cfg.Source.SlotName, r.Cfg.Source.PublicationName); err != nil {
		return err
	}

	plan, err := BuildPlan(ctx, r.Src, &r.Cfg.Filter)
	if err != nil {
		return err
	}
	if len(plan.Units) == 0 {
		return errors.New("the filter selects no table that holds rows")
	}
	r.Logger.Info("snapshot plan", "tables", len(plan.All), "copy_units", len(plan.Units), "text_format_tables", len(plan.TextFallbacks))

	var archive string
	if !r.Cfg.Snapshot.SkipDDL {
		file, cleanup, err := schema.TempFile()
		if err != nil {
			return err
		}
		defer cleanup()
		archive = file
		if err := r.importPreData(ctx, plan, archive, restarted); err != nil {
			return err
		}
	}

	if err := store.UpsertTables(ctx, r.Dst, toProgress(plan.Units)); err != nil {
		return err
	}

	slot, err := CreateSlot(ctx, pg.ReplicationDSN(r.Cfg.Source.DSN, r.Cfg.Source.ReplicationDSN), r.Cfg.Source.SlotName, r.Logger)
	if err != nil {
		return err
	}
	// The snapshot is released on every path; the slot itself stays.
	defer func() { _ = slot.Close(context.WithoutCancel(ctx)) }()
	if err := store.SetSnapshotPoint(ctx, r.Dst, slot.ConsistentLSN); err != nil {
		return err
	}

	copier, err := NewCopier(r.Cfg, r.Dst, r.Logger)
	if err != nil {
		return err
	}
	stats, err := copier.Run(ctx, slot.SnapshotName, plan.Units)
	if err != nil {
		return fmt.Errorf("the copy failed after %d of %d tables (%s): %w\n"+
			"Look at 'gophant status' for the failed table; after fixing the cause run 'gophant snapshot --restart'",
			stats.Tables, len(plan.Units), stats.Duration.Round(time.Second), err)
	}
	if err := slot.Close(ctx); err != nil {
		r.Logger.Warn("closing the snapshot connection failed", "error", err)
	}

	if !r.Cfg.Snapshot.SkipDDL {
		if err := r.importPostData(ctx, archive, restarted); err != nil {
			return fmt.Errorf("the data was copied but the post-data import failed: %w\n"+
				"After fixing the cause run 'gophant snapshot' again: it only repeats the post-data import", err)
		}
	}
	if err := store.SetStage(ctx, r.Dst, meta.StageCDCStream); err != nil {
		return err
	}
	r.summary(slot, stats, time.Since(started))
	return nil
}

// importPreData dumps the schema of s1 into archive and imports the pre-data section on s3. A restart
// whose tables are all still on s3 skips the import.
func (r *Runner) importPreData(ctx context.Context, plan *Plan, archive string, restarted bool) error {
	runner := schema.NewRunner(r.Cfg, r.Logger)
	if err := runner.Dump(ctx, archive); err != nil {
		return err
	}
	existing, err := existingTables(ctx, r.Dst, plan.All)
	if err != nil {
		return err
	}
	if restarted && len(existing) == len(plan.All) {
		r.Logger.Info("the tables of the previous attempt are still on s3, skipping the pre-data import")
		return nil
	}
	schemas, err := pg.QueryStrings(ctx, r.Dst, `SELECT nspname FROM pg_namespace`)
	if err != nil {
		return fmt.Errorf("list the schemas of s3: %w", err)
	}
	return runner.RestorePreData(ctx, archive, schemas)
}

// importPostData creates the indexes, constraints and triggers on s3. After a restart or a failed
// attempt some of them exist already and are left alone; on a fresh target nothing is filtered.
func (r *Runner) importPostData(ctx context.Context, archive string, mayExist bool) error {
	var existing *schema.ExistingObjects
	if mayExist {
		var err error
		if existing, err = schema.LoadExistingObjects(ctx, r.Dst); err != nil {
			return err
		}
	}
	return schema.NewRunner(r.Cfg, r.Logger).RestorePostData(ctx, archive, r.Cfg.Snapshot.Workers, existing)
}

// finishPostData continues a snapshot whose tables are all copied but whose post-data import did not
// complete: the schema is dumped again (the DDL is frozen) and the post-data objects are recreated.
func (r *Runner) finishPostData(ctx context.Context, store *meta.Store, state *meta.SyncState) error {
	r.Logger.Info("all tables were copied by a previous run, repeating the post-data import",
		"consistent_lsn", state.ConsistentLSN.String())
	if !r.Cfg.Snapshot.SkipDDL {
		file, cleanup, err := schema.TempFile()
		if err != nil {
			return err
		}
		defer cleanup()
		if err := schema.NewRunner(r.Cfg, r.Logger).Dump(ctx, file); err != nil {
			return err
		}
		if err := r.importPostData(ctx, file, true); err != nil {
			return fmt.Errorf("the post-data import failed again: %w", err)
		}
	}
	if err := store.SetStage(ctx, r.Dst, meta.StageCDCStream); err != nil {
		return err
	}
	fmt.Fprintf(r.Out, "\nPost-data imported. Next step: run \"gophant sync\" to replay the WAL stream from %s.\n", state.ConsistentLSN)
	return nil
}

func (r *Runner) confirmRestart(ctx context.Context, store *meta.Store) (bool, error) {
	tables, err := store.ListTables(ctx, r.Dst)
	if err != nil {
		return false, err
	}
	prompt := fmt.Sprintf("--restart will drop the replication slot %q on s1, TRUNCATE the %d table(s) of the previous attempt on s3 "+
		"and reset %s. Continue?", r.Cfg.Source.SlotName, len(tables), r.Cfg.Target.MetaSchema)
	return r.Confirm(prompt)
}

// discard removes the state of the previous attempt: the slot on s1, the copied rows on s3 and the
// meta rows. The tables and their schema stay on s3.
func (r *Runner) discard(ctx context.Context, store *meta.Store, state *meta.SyncState) error {
	if state != nil && state.SlotName != "" && state.SlotName != r.Cfg.Source.SlotName {
		return fmt.Errorf("the migration state refers to slot %q but source.slot_name is %q", state.SlotName, r.Cfg.Source.SlotName)
	}
	if err := DropSlot(ctx, r.Src, r.Cfg.Source.SlotName, r.Logger); err != nil {
		return err
	}

	tables, err := store.ListTables(ctx, r.Dst)
	if err != nil {
		return err
	}
	var quoted []string
	for _, t := range tables {
		ok, err := tableExists(ctx, r.Dst, t.Schema, t.Name)
		if err != nil {
			return err
		}
		if ok {
			quoted = append(quoted, pg.Qualified(t.Schema, t.Name))
		}
	}
	if len(quoted) > 0 {
		// One statement for all tables: it also works when foreign keys between them exist.
		r.Logger.Info("truncating the tables of the previous attempt", "tables", len(quoted))
		if _, err := r.Dst.Exec(ctx, "TRUNCATE "+strings.Join(quoted, ", ")); err != nil {
			return fmt.Errorf("truncate the tables of the previous attempt: %w", err)
		}
	}
	return store.ResetState(ctx, r.Dst)
}

func (r *Runner) summary(slot *Slot, stats Stats, total time.Duration) {
	fmt.Fprintf(r.Out, "\nSnapshot complete.\n")
	fmt.Fprintf(r.Out, "  tables copied  : %d\n", stats.Tables)
	fmt.Fprintf(r.Out, "  rows copied    : %s\n", format.Int(stats.Rows))
	fmt.Fprintf(r.Out, "  data streamed  : %s\n", format.Bytes(stats.Bytes))
	fmt.Fprintf(r.Out, "  copy duration  : %s\n", stats.Duration.Round(time.Millisecond))
	fmt.Fprintf(r.Out, "  total duration : %s\n", total.Round(time.Millisecond))
	fmt.Fprintf(r.Out, "  consistent_lsn : %s\n", slot.ConsistentLSN)
	fmt.Fprintf(r.Out, "Next step: run \"gophant sync\" to replay the WAL stream from the consistent point.\n")
}

func toProgress(tables []catalog.Table) []meta.TableProgress {
	out := make([]meta.TableProgress, len(tables))
	for i, t := range tables {
		out[i] = meta.TableProgress{Schema: t.Schema, Name: t.Name, HasPK: t.HasPK, ReplicaID: string(t.ReplicaIdent)}
	}
	return out
}

// existingTables returns the tables of the list that already exist on the database behind q.
func existingTables(ctx context.Context, q pg.Querier, tables []catalog.Table) (map[string]bool, error) {
	schemas := map[string]bool{}
	for _, t := range tables {
		schemas[t.Schema] = true
	}
	list := make([]string, 0, len(schemas))
	for s := range schemas {
		list = append(list, s)
	}
	names, err := pg.QueryStrings(ctx, q, `SELECT n.nspname || '.' || c.relname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p') AND n.nspname = ANY ($1::text[])`, list)
	if err != nil {
		return nil, fmt.Errorf("look for existing tables on s3: %w", err)
	}
	wanted := make(map[string]bool, len(tables))
	for _, t := range tables {
		wanted[t.FullName()] = true
	}
	found := map[string]bool{}
	for _, n := range names {
		if wanted[n] {
			found[n] = true
		}
	}
	return found, nil
}

func tableExists(ctx context.Context, q pg.Querier, schemaName, name string) (bool, error) {
	var ok bool
	if err := q.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, pg.Qualified(schemaName, name)).Scan(&ok); err != nil {
		return false, fmt.Errorf("look for %s.%s on s3: %w", schemaName, name, err)
	}
	return ok, nil
}
