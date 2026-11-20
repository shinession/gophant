// Package cutover runs the CUTOVER stage inside the downtime window: it waits until s3 has applied
// everything s1 wrote, copies the sequence positions and moves the migration to verification.
package cutover

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/meta"
	"github.com/shinession/gophant/internal/sequence"
)

const (
	// requiredStable is the number of consecutive polls that must confirm that s3 has caught up.
	requiredStable = 3
	defaultPoll    = time.Second
	reportEvery    = 5 * time.Second
)

// Runner performs the cutover.
type Runner struct {
	Cfg    *config.Config
	Src    *pgxpool.Pool
	Dst    *pgxpool.Pool
	Logger *slog.Logger
	Out    io.Writer
	// Confirm asks the operator to confirm that s1 is no longer written. It returns false to cancel.
	Confirm func(prompt string) (bool, error)
	// PollInterval is the time between two checks of the lag; one second when zero.
	PollInterval time.Duration
}

// Run waits for lag zero, synchronizes the sequences and sets the stage to VERIFY.
//
// The writes to s1 must already be stopped; gophant does not stop them. The command refuses to go on
// while a session of s1 still holds a write transaction, and while no gophant sync is consuming the
// slot, because then the lag can never reach zero.
func (r *Runner) Run(ctx context.Context) error {
	store := meta.NewStore(r.Cfg.Target.MetaSchema)
	state, err := store.GetState(ctx, r.Dst)
	if err != nil {
		return err
	}
	if state == nil {
		return errors.New("no migration state found on s3: run 'gophant snapshot' first")
	}
	switch state.Stage {
	case meta.StageCDCStream, meta.StageCutover:
	default:
		return fmt.Errorf("the cutover needs the stage %s or %s, but the migration is in stage %s", meta.StageCDCStream, meta.StageCutover, state.Stage)
	}

	active, err := r.slotActive(ctx)
	if err != nil {
		return err
	}
	if !active {
		return fmt.Errorf("replication slot %s is not being consumed: start 'gophant sync' first and run the cutover again, the lag cannot reach zero without it", r.Cfg.Source.SlotName)
	}

	ok, err := r.Confirm("The cutover starts the downtime window. Have ALL writes to s1 been stopped " +
		"(application stopped or the database set read-only, no transaction left open)?")
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("cutover cancelled")
	}

	if state.Stage != meta.StageCutover {
		if err := store.SetStage(ctx, r.Dst, meta.StageCutover); err != nil {
			return err
		}
	}
	started := time.Now()
	fmt.Fprintln(r.Out, "Cutover started: waiting for s3 to apply everything s1 wrote.")

	if err := r.waitForLagZero(ctx, store); err != nil {
		return err
	}
	fmt.Fprintf(r.Out, "Lag is zero after %s.\n", time.Since(started).Round(time.Millisecond))

	res, err := sequence.Sync(ctx, r.Src, r.Dst, &r.Cfg.Filter, r.Cfg.Cutover.SequenceOffset, r.Logger)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.Out, "%d sequence(s) synchronized (offset %d).\n", res.Synced, r.Cfg.Cutover.SequenceOffset)
	for _, name := range res.Missing {
		fmt.Fprintf(r.Out, "  warning: the sequence %s does not exist on s3 and was skipped\n", name)
	}

	if err := store.SetStage(ctx, r.Dst, meta.StageVerify); err != nil {
		return err
	}
	fmt.Fprintf(r.Out, "\nCutover complete in %s. The migration is now in stage %s; gophant sync stops by itself.\n",
		time.Since(started).Round(time.Millisecond), meta.StageVerify)
	fmt.Fprintln(r.Out, "Next step: run \"gophant verify\" while s1 stays write-stopped.")
	return nil
}

// waitForLagZero polls until s3 has applied the WAL of s1, with nobody writing to s1.
//
// "Caught up" is not "applied_lsn equals pg_current_wal_lsn() right now": s1 keeps writing small
// records of its own (checkpoints, autovacuum) after the last transaction, so the position moves under
// the poll. The condition is that applied_lsn reached the WAL position that s1 had one poll earlier,
// with no open write transaction and no prepared transaction, for several polls in a row. An idle
// stream moves applied_lsn forward on its own (the sync reports it with every keepalive), so a quiet
// s1 satisfies this within a few seconds.
func (r *Runner) waitForLagZero(ctx context.Context, store *meta.Store) error {
	poll := r.PollInterval
	if poll <= 0 {
		poll = defaultPoll
	}
	canSee, err := r.canSeeSessions(ctx)
	if err != nil {
		return err
	}
	if !canSee {
		r.Logger.Warn("the source role cannot see the sessions of other users (pg_read_all_stats is missing): " +
			"gophant cannot check for open write transactions and relies on your confirmation")
		fmt.Fprintln(r.Out, "  warning: open write transactions on s1 cannot be detected with this role.")
	}

	deadline := time.Now().Add(r.Cfg.Cutover.LagZeroTimeout.Std())
	var previous pglogrepl.LSN
	var lastReport time.Time
	stable := 0
	for {
		var writers, prepared int64
		if canSee {
			if writers, prepared, err = r.openWriters(ctx); err != nil {
				return err
			}
		}
		current, err := r.currentWAL(ctx)
		if err != nil {
			return err
		}
		state, err := store.GetState(ctx, r.Dst)
		if err != nil {
			return err
		}
		applied, _ := state.StartLSN()

		caughtUp := previous != 0 && applied >= previous && writers == 0 && prepared == 0
		if caughtUp {
			stable++
		} else {
			stable = 0
		}
		previous = current
		if stable >= requiredStable {
			return nil
		}

		if time.Since(lastReport) >= reportEvery {
			lastReport = time.Now()
			fmt.Fprintf(r.Out, "  waiting: lag %d bytes, %d open write transaction(s), %d prepared transaction(s)\n",
				lagBytes(current, applied), writers, prepared)
		}
		if time.Now().After(deadline) {
			active, _ := r.slotActive(ctx)
			return fmt.Errorf("the lag did not reach zero within %s: lag %d bytes, %d open write transaction(s), %d prepared transaction(s), "+
				"gophant sync consuming the slot: %t. The migration stays in stage %s, run the cutover again once the cause is fixed",
				r.Cfg.Cutover.LagZeroTimeout.Std(), lagBytes(current, applied), writers, prepared, active, meta.StageCutover)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func lagBytes(current, applied pglogrepl.LSN) int64 {
	if current <= applied {
		return 0
	}
	return int64(current - applied)
}

func (r *Runner) currentWAL(ctx context.Context) (pglogrepl.LSN, error) {
	var text string
	if err := r.Src.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&text); err != nil {
		return 0, fmt.Errorf("read pg_current_wal_lsn() on s1: %w", err)
	}
	lsn, err := pglogrepl.ParseLSN(text)
	if err != nil {
		return 0, err
	}
	return lsn, nil
}

// slotActive reports whether a connection is consuming the replication slot.
func (r *Runner) slotActive(ctx context.Context) (bool, error) {
	var active bool
	err := r.Src.QueryRow(ctx, `SELECT COALESCE((SELECT active FROM pg_replication_slots WHERE slot_name = $1), false)`,
		r.Cfg.Source.SlotName).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("look up the replication slot on s1: %w", err)
	}
	return active, nil
}

// canSeeSessions reports whether the source role sees the transaction ids of other users' sessions.
func (r *Runner) canSeeSessions(ctx context.Context) (bool, error) {
	var ok bool
	err := r.Src.QueryRow(ctx, `SELECT pg_has_role(current_user, 'pg_read_all_stats', 'USAGE')
       OR COALESCE((SELECT rolsuper FROM pg_roles WHERE rolname = current_user), false)`).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check the monitoring privileges on s1: %w", err)
	}
	return ok, nil
}

// openWriters counts the client sessions that hold a transaction with an assigned xid (it wrote
// something and has not ended yet) and the prepared transactions. Either can still commit changes
// that the stream has not seen yet.
func (r *Runner) openWriters(ctx context.Context) (writers, prepared int64, err error) {
	err = r.Src.QueryRow(ctx, `SELECT
  (SELECT count(*) FROM pg_stat_activity
    WHERE backend_type = 'client backend' AND backend_xid IS NOT NULL AND pid <> pg_backend_pid()),
  (SELECT count(*) FROM pg_prepared_xacts)`).Scan(&writers, &prepared)
	if err != nil {
		return 0, 0, fmt.Errorf("look for open write transactions on s1: %w", err)
	}
	return writers, prepared, nil
}
