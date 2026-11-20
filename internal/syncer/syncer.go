// Package syncer runs the CDC_STREAM stage: it starts the WAL reader and the applier, connects them,
// restarts both from the persisted position after a transient failure and shuts them down in order.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shinession/gophant/internal/applier"
	"github.com/shinession/gophant/internal/cdc"
	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/meta"
	"github.com/shinession/gophant/internal/pg"
	"golang.org/x/sync/errgroup"
)

const (
	initialBackoff = time.Second
	maxBackoff     = 30 * time.Second
	// stableRun is how long an attempt has to run before the backoff starts from the beginning again.
	stableRun = 5 * time.Minute
	// watchInterval is how often the stage is read from s3.
	watchInterval = time.Second
	// reportInterval is how often a progress line is logged.
	reportInterval = 10 * time.Second
)

var (
	// errCutoverDone stops the stream after the cutover has completed.
	errCutoverDone = errors.New("the cutover is complete")
	// errFinished is returned by an attempt that found nothing left to do.
	errFinished = errors.New("nothing left to sync")
)

// Syncer runs the stream.
type Syncer struct {
	Cfg    *config.Config
	Src    *pgxpool.Pool
	Dst    *pgxpool.Pool
	Logger *slog.Logger
	Out    io.Writer
}

// Run consumes the WAL stream until ctx is cancelled (a graceful shutdown that finishes the batch in
// progress), the cutover completes, or a permanent error occurs. A transient error, such as a lost
// connection, restarts the stream from the position stored in s3, which is what makes the replay
// resumable without gap and without duplicate.
func (s *Syncer) Run(ctx context.Context) error {
	backoff := initialBackoff
	for {
		started := time.Now()
		err := s.runOnce(ctx)
		switch {
		case err == nil || errors.Is(err, errFinished):
			return nil
		case ctx.Err() != nil:
			return nil
		case !pg.IsRetryable(err):
			return err
		}
		if time.Since(started) >= stableRun {
			backoff = initialBackoff
		}
		s.Logger.Warn("the stream stopped, it will be resumed from the position stored on s3", "error", err, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// runOnce makes one attempt: it reads the state, checks s1, and runs reader and applier until one of
// them ends.
func (s *Syncer) runOnce(ctx context.Context) error {
	store := meta.NewStore(s.Cfg.Target.MetaSchema)
	state, err := store.GetState(ctx, s.Dst)
	if err != nil {
		return err
	}
	if state == nil {
		return errors.New("no migration state found on s3: run 'gophant snapshot' first")
	}
	switch state.Stage {
	case meta.StageCDCStream, meta.StageCutover:
	case meta.StageVerify, meta.StageDone:
		fmt.Fprintf(s.Out, "The migration is in stage %s: the cutover is complete and there is nothing left to sync.\n", state.Stage)
		return errFinished
	default:
		return fmt.Errorf("the snapshot is not finished (stage %s): run 'gophant snapshot' first", state.Stage)
	}
	start, ok := state.StartLSN()
	if !ok {
		return errors.New("the migration state has neither applied_lsn nor consistent_lsn")
	}
	if err := s.preflight(ctx, start); err != nil {
		return err
	}

	progress := &cdc.Progress{}
	progress.AdvanceFlushed(start)
	progress.AdvanceWritten(start)

	reader := cdc.NewReader(s.Cfg, progress, s.Logger)
	app, err := applier.New(s.Cfg, progress, s.Logger)
	if err != nil {
		return err
	}

	events := make(chan cdc.Event, s.Cfg.CDC.BatchMaxTxns)
	applierDone := make(chan struct{})

	// readerCtx ends on a shutdown signal, on an applier failure and when the cutover completes. The
	// applier deliberately does not use ctx: after the reader stopped it still has to commit the
	// transactions that were already received.
	readerCtx, stopReader := context.WithCancelCause(ctx)
	defer stopReader(nil)

	var readerErr, applierErr error
	g, _ := errgroup.WithContext(context.Background())
	g.Go(func() error {
		readerErr = reader.Run(readerCtx, start, events, applierDone)
		// Whatever ended the stream, the stage watcher has nothing left to watch. The first cause wins,
		// so a cutover that already stopped the reader stays recognizable.
		stopReader(readerErr)
		return nil
	})
	g.Go(func() error {
		applierErr = app.Run(context.WithoutCancel(ctx), events)
		close(applierDone)
		if applierErr != nil {
			stopReader(applierErr)
		}
		// Release the transactions that were queued but not applied (their spill files).
		for ev := range events {
			if ev.Tx != nil {
				ev.Tx.Done()
			}
		}
		return nil
	})
	g.Go(func() error {
		s.watch(readerCtx, store, progress, app, stopReader)
		return nil
	})
	// The goroutines report through readerErr and applierErr and always return nil, so Wait only waits.
	_ = g.Wait()

	switch {
	case errors.Is(context.Cause(readerCtx), errCutoverDone):
		fmt.Fprintln(s.Out, "The cutover is complete: the stream was stopped.")
		return errFinished
	case applierErr != nil:
		return applierErr
	default:
		return readerErr
	}
}

// preflight verifies on s1 that the stream can continue from start without a gap.
func (s *Syncer) preflight(ctx context.Context, start pglogrepl.LSN) error {
	var (
		plugin, walStatus, confirmedText string
		active                           bool
	)
	err := s.Src.QueryRow(ctx, `SELECT plugin, COALESCE(wal_status, ''), COALESCE(confirmed_flush_lsn::text, ''), active
FROM pg_replication_slots WHERE slot_name = $1`, s.Cfg.Source.SlotName).Scan(&plugin, &walStatus, &confirmedText, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("replication slot %s does not exist on s1: the stream cannot continue without gap, run 'gophant snapshot --restart'", s.Cfg.Source.SlotName)
	}
	if err != nil {
		return fmt.Errorf("check the replication slot: %w", err)
	}
	if walStatus == "lost" {
		return fmt.Errorf("replication slot %s was invalidated (wal_status = lost, max_slot_wal_keep_size was exceeded): run 'gophant snapshot --restart'", s.Cfg.Source.SlotName)
	}
	if plugin != "pgoutput" {
		return fmt.Errorf("replication slot %s uses plugin %q, gophant needs pgoutput", s.Cfg.Source.SlotName, plugin)
	}
	if confirmedText != "" {
		confirmed, err := pglogrepl.ParseLSN(confirmedText)
		if err != nil {
			return fmt.Errorf("parse confirmed_flush_lsn %q: %w", confirmedText, err)
		}
		// s1 only moves the slot after s3 reported a position, and s3 reports only what it has
		// committed. A slot that is ahead of s3 means s3 lost state (a restore from a backup, a reset
		// of the meta tables): the WAL in between is gone and starting here would silently skip it.
		if confirmed > start {
			return fmt.Errorf("slot %s on s1 has confirmed WAL up to %s but s3 only applied up to %s: s3 lost progress and the missing changes cannot be replayed; "+
				"run 'gophant snapshot --restart'", s.Cfg.Source.SlotName, confirmed, start)
		}
	}
	if active {
		s.Logger.Warn("the replication slot is still marked active on s1, probably by a previous connection that has not timed out yet", "slot", s.Cfg.Source.SlotName)
	}
	return nil
}

// watch polls the stage: it speeds up the heartbeat while the cutover waits for lag zero, stops the
// stream when the cutover is complete, and logs the progress.
func (s *Syncer) watch(ctx context.Context, store *meta.Store, progress *cdc.Progress, app *applier.Applier, stop context.CancelCauseFunc) {
	tick := time.NewTicker(watchInterval)
	defer tick.Stop()
	lastReport := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		state, err := store.GetState(ctx, s.Dst)
		if err != nil {
			s.Logger.Debug("could not read the stage", "error", err)
		} else if state != nil {
			progress.SetFast(state.Stage == meta.StageCutover)
			if state.Stage == meta.StageVerify || state.Stage == meta.StageDone {
				stop(errCutoverDone)
				return
			}
		}
		if time.Since(lastReport) >= reportInterval {
			lastReport = time.Now()
			st := &app.Stats
			s.Logger.Info("progress",
				"flushed_lsn", progress.Flushed().String(), "received_lsn", progress.Written().String(),
				"in_flight_txs", progress.InFlight(),
				"transactions", st.Txs.Load(), "batches", st.Batches.Load(),
				"inserts", st.Inserts.Load(), "updates", st.Updates.Load(), "deletes", st.Deletes.Load(), "truncates", st.Truncates.Load())
		}
	}
}
