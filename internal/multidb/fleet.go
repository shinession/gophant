package multidb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/meta"
	"github.com/shinession/gophant/internal/pg"
)

// Database is one database of the migration.
type Database struct {
	Name string
	// CanConnect says whether the source role may connect to it.
	CanConnect bool
	// Cfg is the configuration of this database, see DatabaseConfig.
	Cfg *config.Config
	// Logger adds the name of the database to every line.
	Logger *slog.Logger
}

// Fleet is the set of databases a command works on, with the two connections it needs to find them.
type Fleet struct {
	Cfg    *config.Config
	Logger *slog.Logger
	// Out receives the reports. The text that a stage prints for one database gets the name of that
	// database in front of every line.
	Out io.Writer
	// Confirm is asked before a step that touches many databases. It returns false to cancel.
	Confirm func(prompt string) (bool, error)

	// Databases are the selected databases in alphabetical order.
	Databases []*Database
	// LeftOut are the databases of s1 that databases.include, databases.exclude or --database left out.
	LeftOut []string

	// src and dst are connected to the databases named in source.dsn and target.dsn. They find the
	// databases and create what is missing; the stages open their own connections to each database.
	src, dst *pgxpool.Pool
	outMu    sync.Mutex
}

// Open connects to s1 and s3, finds the databases of s1 and applies databases.include,
// databases.exclude and the names given with --database (only). appName shows up in pg_stat_activity.
func Open(ctx context.Context, cfg *config.Config, logger *slog.Logger, out io.Writer,
	confirm func(prompt string) (bool, error), only []string, appName string) (*Fleet, error) {
	src, err := pg.NewPool(ctx, cfg.Source.DSN, appName, 4)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	dst, err := pg.NewPool(ctx, cfg.Target.DSN, appName, 4)
	if err != nil {
		src.Close()
		return nil, fmt.Errorf("target: %w", err)
	}
	f := &Fleet{Cfg: cfg, Logger: logger, Out: out, Confirm: confirm, src: src, dst: dst}
	if err := f.discover(ctx, only); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (f *Fleet) discover(ctx context.Context, only []string) error {
	found, err := Discover(ctx, f.src)
	if err != nil {
		return err
	}
	sel, err := Select(found, &f.Cfg.Databases, only)
	if err != nil {
		return err
	}
	for _, s := range sel.Selected {
		dbCfg, err := DatabaseConfig(f.Cfg, s.Name)
		if err != nil {
			return fmt.Errorf("database %q: %w", s.Name, err)
		}
		f.Databases = append(f.Databases, &Database{
			Name: s.Name, CanConnect: s.CanConnect, Cfg: dbCfg, Logger: f.Logger.With("db", s.Name),
		})
	}
	f.LeftOut = sel.LeftOut
	f.Logger.Info("databases of s1", "selected", len(f.Databases), "left_out", len(f.LeftOut))
	return nil
}

// Close releases the two connections.
func (f *Fleet) Close() {
	f.src.Close()
	f.dst.Close()
}

// outFor returns the writer that stages use for one database. Call Flush on it when the stage ended.
func (f *Fleet) outFor(d *Database) *lineWriter { return newLineWriter(&f.outMu, f.Out, d.Name) }

// openPools opens a connection pool to the copy of the database on s1 and one to the copy on s3.
func openPools(ctx context.Context, d *Database, appName string) (src, dst *pgxpool.Pool, err error) {
	src, err = pg.NewPool(ctx, d.Cfg.Source.DSN, appName, 4)
	if err != nil {
		return nil, nil, fmt.Errorf("source: %w", err)
	}
	dst, err = pg.NewPool(ctx, d.Cfg.Target.DSN, appName, 4)
	if err != nil {
		src.Close()
		return nil, nil, fmt.Errorf("target: %w", err)
	}
	return src, dst, nil
}

// targetDatabases returns the names of the databases that s3 has.
func (f *Fleet) targetDatabases(ctx context.Context) (map[string]bool, error) {
	names, err := pg.QueryStrings(ctx, f.dst, `SELECT datname FROM pg_database`)
	if err != nil {
		return nil, fmt.Errorf("list the databases of s3: %w", err)
	}
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set, nil
}

// state reads the state of the migration of one database from s3: nil when the migration has not
// started, which includes a database that s3 does not have yet.
func (f *Fleet) state(ctx context.Context, d *Database, onTarget map[string]bool) (*meta.SyncState, error) {
	if !onTarget[d.Name] {
		return nil, nil
	}
	conn, err := pg.Connect(ctx, d.Cfg.Target.DSN, "gophant-state")
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	return meta.NewStore(d.Cfg.Target.MetaSchema).GetState(ctx, conn)
}

// states reads the state of every database in dbs.
func (f *Fleet) states(ctx context.Context, dbs []*Database) (map[string]*meta.SyncState, error) {
	onTarget, err := f.targetDatabases(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*meta.SyncState, len(dbs))
	for _, d := range dbs {
		st, err := f.state(ctx, d, onTarget)
		if err != nil {
			return nil, fmt.Errorf("database %q: %w", d.Name, err)
		}
		out[d.Name] = st
	}
	return out, nil
}

// Outcome is how a stage ended for one database.
type Outcome struct {
	Database *Database
	Err      error
	// Note replaces the word "ok" in the summary, for example "skipped: already complete".
	Note string
}

// forEach runs fn for every database, at most parallel at a time, and waits for all of them. A
// failure ends only the database it belongs to. The databases are started in order; when ctx is
// cancelled the ones that have not started yet end with the error of the context.
func forEach(ctx context.Context, dbs []*Database, parallel int, fn func(ctx context.Context, d *Database) (note string, err error)) []Outcome {
	out := make([]Outcome, len(dbs))
	sem := make(chan struct{}, max(parallel, 1))
	var wg sync.WaitGroup
	for i, d := range dbs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			out[i] = Outcome{Database: d, Err: ctx.Err()}
			continue
		}
		wg.Go(func() {
			defer func() { <-sem }()
			note, err := fn(ctx, d)
			out[i] = Outcome{Database: d, Err: err, Note: note}
		})
	}
	wg.Wait()
	return out
}

// summarize prints one line per database, in alphabetical order, and returns an error when any of
// them failed.
func summarize(w io.Writer, stage string, outs []Outcome) error {
	outs = slices.Clone(outs)
	slices.SortFunc(outs, func(a, b Outcome) int { return strings.Compare(a.Database.Name, b.Database.Name) })
	width := 0
	failed := 0
	for _, o := range outs {
		width = max(width, len(o.Database.Name))
		if o.Err != nil {
			failed++
		}
	}
	fmt.Fprintf(w, "\nSummary of %s: %d database(s), %d ok, %d failed\n", stage, len(outs), len(outs)-failed, failed)
	var names []string
	for _, o := range outs {
		switch {
		case o.Err != nil:
			fmt.Fprintf(w, "  %-*s  FAILED: %s\n", width, o.Database.Name, firstLine(o.Err))
			names = append(names, o.Database.Name)
		case o.Note != "":
			fmt.Fprintf(w, "  %-*s  %s\n", width, o.Database.Name, o.Note)
		default:
			fmt.Fprintf(w, "  %-*s  ok\n", width, o.Database.Name)
		}
	}
	if failed == 0 {
		return nil
	}
	return fmt.Errorf("%s failed for %d of %d database(s): %s", stage, failed, len(outs), strings.Join(names, ", "))
}

// firstLine returns the first line of an error; a multi-line error repeats what the log has already said.
func firstLine(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}

// errCancelled is returned when the operator answers no.
var errCancelled = errors.New("cancelled")
