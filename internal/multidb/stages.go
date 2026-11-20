package multidb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/shinession/gophant/internal/cutover"
	"github.com/shinession/gophant/internal/format"
	"github.com/shinession/gophant/internal/meta"
	"github.com/shinession/gophant/internal/snapshot"
	"github.com/shinession/gophant/internal/syncer"
	"github.com/shinession/gophant/internal/verify"
)

// phase says where the migration of a database stands, which decides what a stage does with it.
type phase int

const (
	// phaseCopy: the migration has not started, or the snapshot is not finished.
	phaseCopy phase = iota
	// phaseStream: the snapshot is done and the WAL stream is replayed (stages CDC_STREAM and CUTOVER).
	phaseStream
	// phaseDone: the cutover is complete (stages VERIFY and DONE).
	phaseDone
)

func phaseOf(st *meta.SyncState) phase {
	if st == nil {
		return phaseCopy
	}
	switch st.Stage {
	case meta.StageCDCStream, meta.StageCutover:
		return phaseStream
	case meta.StageVerify, meta.StageDone:
		return phaseDone
	}
	return phaseCopy
}

// yes answers the question of a single-database stage that the operator has already answered for all databases.
func yes(string) (bool, error) { return true, nil }

// startable decides which databases of a precheck may start. A failure of the cluster stops
// everything. A database with a failure of its own is skipped, after the operator agreed, when
// something else can go on: another database that is ready, or streams (streams) that are running.
func (f *Fleet) startable(rep *Report, streams int) (ready []*Database, others []Outcome, err error) {
	if rep.ClusterFailed() {
		return nil, nil, errors.New("the precheck found failures in the cluster: fix them before any database can start")
	}
	for _, d := range rep.Skipped() {
		others = append(others, Outcome{Database: d.Database, Note: "skipped: " + d.Skipped})
	}
	for _, d := range rep.Ready() {
		ready = append(ready, d.Database)
	}
	blocked := rep.Blocked()
	if len(blocked) == 0 {
		return ready, others, nil
	}
	names := make([]string, len(blocked))
	for i, b := range blocked {
		names[i] = b.Database.Name
		others = append(others, Outcome{Database: b.Database, Err: errors.New("blocked by the precheck")})
	}
	if len(ready) == 0 && streams == 0 {
		return nil, others, fmt.Errorf("no database can start: fix the failures above (%s)", strings.Join(names, ", "))
	}
	ok, err := f.Confirm(fmt.Sprintf("%d database(s) have failures above and will be skipped: %s. Continue with the others?", len(blocked), strings.Join(names, ", ")))
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, errCancelled
	}
	return ready, others, nil
}

// Snapshot runs the snapshot stage for the selected databases: the precheck of all of them, then, for
// the ones that passed, the creation of what is missing, and the snapshot, databases.parallel at a
// time. A database whose snapshot is complete is left alone unless restart is set.
func (f *Fleet) Snapshot(ctx context.Context, restart bool) error {
	states, err := f.states(ctx, f.Databases)
	if err != nil {
		return err
	}
	var todo []*Database
	var outs []Outcome
	for _, d := range f.Databases {
		if st := states[d.Name]; !restart && phaseOf(st) != phaseCopy {
			outs = append(outs, Outcome{Database: d, Note: fmt.Sprintf("skipped: the snapshot is already complete (stage %s)", st.Stage)})
			continue
		}
		todo = append(todo, d)
	}
	if len(todo) == 0 {
		return summarize(f.Out, "snapshot", outs)
	}
	if restart {
		ok, err := f.Confirm(fmt.Sprintf("--restart will drop the replication slot on s1, TRUNCATE the copied tables on s3 and reset %s "+
			"for each of the %d selected database(s), and copy them again. Continue?", f.Cfg.Target.MetaSchema, len(todo)))
		if err != nil {
			return err
		}
		if !ok {
			return errCancelled
		}
	}

	rep, err := f.Precheck(ctx, todo, todo)
	if err != nil {
		return err
	}
	rep.Print(f.Out)
	ready, others, err := f.startable(rep, 0)
	if err != nil {
		return err
	}
	outs = append(outs, others...)
	outs = append(outs, forEach(ctx, ready, f.Cfg.Databases.Parallel, func(ctx context.Context, d *Database) (string, error) {
		return "", f.snapshotOne(ctx, d, restart)
	})...)
	return summarize(f.Out, "snapshot", outs)
}

// snapshotOne creates what the database is missing on s3 and s1 and runs the snapshot stage for it.
// The precheck has been done for all databases together, so the stage does not repeat it.
func (f *Fleet) snapshotOne(ctx context.Context, d *Database, restart bool) error {
	out := f.outFor(d)
	defer out.Flush()
	if err := f.prepare(ctx, d); err != nil {
		return err
	}
	src, dst, err := openPools(ctx, d, "gophant-snapshot")
	if err != nil {
		return err
	}
	defer src.Close()
	defer dst.Close()
	r := &snapshot.Runner{Cfg: d.Cfg, Src: src, Dst: dst, Logger: d.Logger, Out: out, Confirm: yes}
	if err := r.Run(ctx, snapshot.Options{Restart: restart, SkipPrecheck: true}); err != nil {
		d.Logger.Error("the snapshot of this database failed; its slot keeps WAL on s1 until 'gophant snapshot --restart --database <name>'", "error", firstLine(err))
		return err
	}
	return nil
}

// Sync replays the WAL stream of every selected database whose snapshot is done, all of them at the
// same time, until they are stopped or each one's cutover is complete. A database that is not ready is
// reported and left out; a database whose stream fails permanently stops alone.
func (f *Fleet) Sync(ctx context.Context) error {
	states, err := f.states(ctx, f.Databases)
	if err != nil {
		return err
	}
	var streams []*Database
	var outs []Outcome
	for _, d := range f.Databases {
		st := states[d.Name]
		switch phaseOf(st) {
		case phaseStream:
			streams = append(streams, d)
		case phaseDone:
			outs = append(outs, Outcome{Database: d, Note: fmt.Sprintf("nothing left to sync (stage %s)", st.Stage)})
		default:
			if empty, err := f.nothingToMigrate(ctx, d); err == nil && empty {
				outs = append(outs, Outcome{Database: d, Note: "skipped: " + skipReasonEmpty})
				continue
			}
			err := errors.New("the snapshot is not finished: run 'gophant snapshot' or 'gophant run' first")
			d.Logger.Warn("this database is not ready and is left out", "error", err)
			outs = append(outs, Outcome{Database: d, Err: err})
		}
	}
	outs = append(outs, forEach(ctx, streams, len(streams), f.streamOne)...)
	return summarize(f.Out, "sync", outs)
}

// streamOne replays the stream of one database until it ends.
func (f *Fleet) streamOne(ctx context.Context, d *Database) (string, error) {
	out := f.outFor(d)
	defer out.Flush()
	src, dst, err := openPools(ctx, d, "gophant-sync")
	if err != nil {
		return "", err
	}
	defer src.Close()
	defer dst.Close()
	s := &syncer.Syncer{Cfg: d.Cfg, Src: src, Dst: dst, Logger: d.Logger, Out: out}
	if err := s.Run(ctx); err != nil {
		d.Logger.Error("the stream of this database stopped; the other databases go on", "error", firstLine(err))
		return "", err
	}
	return "", nil
}

// job is one database of the pipeline.
type job struct {
	db *Database
	// copy says that the snapshot stage has to run before the stream.
	copy bool
}

// pipeline runs the snapshot of every database that needs one, at most parallel at a time, and starts
// the stream of a database as soon as its own snapshot is done, while the next databases are still
// being copied. The streams that can start at once start first. A stream does not count against
// parallel, and a failed snapshot does not stop the others.
//
// This is what keeps the WAL that s1 holds for the slots small: a slot only waits for its own
// database to be copied, not for all of them.
func pipeline(ctx context.Context, jobs []job, parallel int,
	copyFn func(context.Context, *Database) error, streamFn func(context.Context, *Database) (string, error)) []Outcome {
	ordered := slices.Clone(jobs)
	slices.SortStableFunc(ordered, func(a, b job) int {
		switch {
		case a.copy == b.copy:
			return 0
		case !a.copy:
			return -1
		}
		return 1
	})

	out := make([]Outcome, len(ordered))
	sem := make(chan struct{}, max(parallel, 1))
	var wg sync.WaitGroup
	for i, j := range ordered {
		if j.copy {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				out[i] = Outcome{Database: j.db, Err: ctx.Err()}
				continue
			}
		}
		wg.Go(func() {
			if j.copy {
				err := copyFn(ctx, j.db)
				<-sem
				if err != nil {
					out[i] = Outcome{Database: j.db, Err: err}
					return
				}
			}
			note, err := streamFn(ctx, j.db)
			out[i] = Outcome{Database: j.db, Err: err, Note: note}
		})
	}
	wg.Wait()
	return out
}

// Run brings every selected database to the replay of its stream and keeps it there: the precheck of
// all databases, then for each database that passed the creation of what is missing, the snapshot and
// the stream, with the snapshots databases.parallel at a time. It ends when the streams end, that is
// when each database has been cut over, or when it is stopped. A database that already streams is
// not copied again, so running the command again after an interruption picks up where it stopped.
func (f *Fleet) Run(ctx context.Context) error {
	states, err := f.states(ctx, f.Databases)
	if err != nil {
		return err
	}
	var check, owners []*Database
	var jobs []job
	var outs []Outcome
	for _, d := range f.Databases {
		st := states[d.Name]
		switch phaseOf(st) {
		case phaseCopy:
			check = append(check, d)
			owners = append(owners, d)
		case phaseStream:
			owners = append(owners, d)
			jobs = append(jobs, job{db: d})
		case phaseDone:
			outs = append(outs, Outcome{Database: d, Note: fmt.Sprintf("nothing to do (stage %s)", st.Stage)})
		}
	}

	if len(check) > 0 {
		rep, err := f.Precheck(ctx, check, owners)
		if err != nil {
			return err
		}
		rep.Print(f.Out)
		ready, others, err := f.startable(rep, len(jobs))
		if err != nil {
			return err
		}
		outs = append(outs, others...)
		for _, d := range ready {
			jobs = append(jobs, job{db: d, copy: true})
		}
	}
	if len(jobs) == 0 {
		return summarize(f.Out, "run", outs)
	}

	f.Logger.Info("starting", "databases", len(jobs), "snapshots_at_a_time", f.Cfg.Databases.Parallel)
	outs = append(outs, pipeline(ctx, jobs, f.Cfg.Databases.Parallel,
		func(ctx context.Context, d *Database) error { return f.snapshotOne(ctx, d, false) },
		f.streamOne)...)
	return summarize(f.Out, "run", outs)
}

// Cutover waits, for every selected database, until s3 has applied everything s1 wrote, then moves the
// sequences and the migration to VERIFY. The writes to s1 are stopped once for all databases, so the
// operator is asked once. A database that is not ready stops the whole command before it starts: a
// cutover that leaves one database behind is not what the operator is waiting for (--database
// limits the cutover to some databases).
func (f *Fleet) Cutover(ctx context.Context) error {
	states, err := f.states(ctx, f.Databases)
	if err != nil {
		return err
	}
	var todo []*Database
	var outs []Outcome
	var notReady []string
	for _, d := range f.Databases {
		st := states[d.Name]
		switch phaseOf(st) {
		case phaseStream:
			todo = append(todo, d)
		case phaseDone:
			outs = append(outs, Outcome{Database: d, Note: fmt.Sprintf("skipped: the cutover is already complete (stage %s)", st.Stage)})
		default:
			// A database without a table never gets a state and is not left behind; one that has tables is.
			empty, err := f.nothingToMigrate(ctx, d)
			switch {
			case err != nil:
				notReady = append(notReady, fmt.Sprintf("%s: %s", d.Name, firstLine(err)))
			case empty:
				outs = append(outs, Outcome{Database: d, Note: "skipped: " + skipReasonEmpty})
			default:
				notReady = append(notReady, d.Name)
			}
		}
	}
	if len(notReady) > 0 {
		return fmt.Errorf("the snapshot of %d database(s) is not finished (%s): finish it first, or limit the cutover with --database",
			len(notReady), strings.Join(notReady, ", "))
	}
	if len(todo) == 0 {
		return summarize(f.Out, "cutover", outs)
	}

	ok, err := f.Confirm(fmt.Sprintf("The cutover starts the downtime window for %d database(s). Have ALL writes to s1 been stopped "+
		"(application stopped or the databases set read-only, no transaction left open)?", len(todo)))
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("cutover cancelled")
	}
	outs = append(outs, forEach(ctx, todo, len(todo), func(ctx context.Context, d *Database) (string, error) {
		return "", f.cutoverOne(ctx, d)
	})...)
	return summarize(f.Out, "cutover", outs)
}

func (f *Fleet) cutoverOne(ctx context.Context, d *Database) error {
	out := f.outFor(d)
	defer out.Flush()
	src, dst, err := openPools(ctx, d, "gophant-cutover")
	if err != nil {
		return err
	}
	defer src.Close()
	defer dst.Close()
	r := &cutover.Runner{Cfg: d.Cfg, Src: src, Dst: dst, Logger: d.Logger, Out: out, Confirm: yes}
	return r.Run(ctx)
}

// Verify compares s1 and s3 for every selected database, databases.parallel at a time. The full
// report is printed for a database with differences, and when only one database is selected; the
// others get one line in the summary. With opts.DropSlot the slot of every verified database is dropped.
func (f *Fleet) Verify(ctx context.Context, opts verify.StageOptions, configPath string) error {
	states, err := f.states(ctx, f.Databases)
	if err != nil {
		return err
	}
	var todo []*Database
	var before []Outcome
	for _, d := range f.Databases {
		if states[d.Name] != nil {
			todo = append(todo, d)
			continue
		}
		// Never started: fine for a database without a table, a failure for one that has tables.
		if empty, err := f.nothingToMigrate(ctx, d); err == nil && empty {
			before = append(before, Outcome{Database: d, Note: "skipped: " + skipReasonEmpty})
		} else {
			before = append(before, Outcome{Database: d, Err: errors.New("the migration of this database has not started: nothing to verify")})
		}
	}

	var mu sync.Mutex
	pending := 0 // verified, slot not dropped yet
	outs := forEach(ctx, todo, f.Cfg.Databases.Parallel, func(ctx context.Context, d *Database) (string, error) {
		var buf bytes.Buffer
		res, err := verify.RunStage(ctx, d.Cfg, d.Logger, &buf, opts)
		if res != nil && (err != nil || len(f.Databases) == 1) {
			out := f.outFor(d)
			_, _ = out.Write(buf.Bytes())
			out.Flush()
		}
		if err != nil {
			return "", err
		}
		var rows int64
		for _, t := range res.Report.Results {
			rows += t.SourceRows
		}
		note := fmt.Sprintf("verified %d table(s), %s rows", len(res.Report.Results), format.Int(rows))
		switch {
		case res.Dropped:
			note += "; the slot was dropped, the migration is DONE"
		case res.Rehearsal:
			note += " (a look only: the migration is not in stage VERIFY yet)"
		default:
			mu.Lock()
			pending++
			mu.Unlock()
		}
		return note, nil
	})
	err = summarize(f.Out, "verify", append(outs, before...))
	if err == nil && pending > 0 {
		fmt.Fprintf(f.Out, "\nSwitch the applications to s3 now. Afterwards release the WAL that s1 keeps for the slots:\n"+
			"  gophant verify --drop-slot -c %s\n", configPath)
	}
	return err
}
