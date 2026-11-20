package snapshot

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shinession/gophant/internal/catalog"
	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/meta"
	"github.com/shinession/gophant/internal/pg"
	"golang.org/x/sync/errgroup"
)

const (
	// maxAttempts is how often one table is tried before the snapshot fails.
	maxAttempts = 3
	// progressInterval is how often the row counter of a running table is written to table_progress.
	progressInterval = 2 * time.Second
	// statusTimeout bounds the bookkeeping writes, which must also work after the context was cancelled.
	statusTimeout = 10 * time.Second
)

// Stats summarizes a finished copy.
type Stats struct {
	Tables   int
	Rows     int64
	Bytes    int64
	Duration time.Duration
}

// Copier copies tables from s1 to s3 with N workers. Every worker owns a pair of connections and
// streams one table at a time: COPY ... TO STDOUT on s1 is piped, without touching the disk, into
// COPY ... FROM STDIN on s3.
type Copier struct {
	cfg    *config.Config
	store  *meta.Store
	dst    pg.Querier // connection pool of s3 for the bookkeeping
	logger *slog.Logger
	srcCfg *pgx.ConnConfig
	dstCfg *pgx.ConnConfig
}

// NewCopier prepares the connection settings of the workers.
func NewCopier(cfg *config.Config, dst pg.Querier, logger *slog.Logger) (*Copier, error) {
	srcCfg, err := pg.ParseConfig(cfg.Source.DSN, "gophant-copy")
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	dstCfg, err := pg.ParseConfig(cfg.Target.DSN, "gophant-copy")
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	return &Copier{
		cfg: cfg, store: meta.NewStore(cfg.Target.MetaSchema), dst: dst, logger: logger,
		srcCfg: srcCfg, dstCfg: dstCfg,
	}, nil
}

// Run copies every table under the exported snapshot. The caller must keep the Slot open until Run
// returns: every table (and every retry of a table) imports the snapshot again.
//
// The first failure cancels the other workers and is returned.
func (c *Copier) Run(ctx context.Context, snapshotName string, tables []catalog.Table) (Stats, error) {
	if !snapshotNamePattern.MatchString(snapshotName) {
		return Stats{}, fmt.Errorf("invalid snapshot name %q", snapshotName)
	}
	start := time.Now()
	var rows, bytes, done atomic.Int64

	workers := min(c.cfg.Snapshot.Workers, len(tables))
	jobs := make(chan catalog.Table)
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		defer close(jobs)
		for _, t := range tables {
			select {
			case jobs <- t:
			case <-gctx.Done():
				return nil
			}
		}
		return nil
	})
	for range workers {
		w := &worker{c: c, snapshot: snapshotName}
		g.Go(func() error {
			defer w.reset()
			for t := range jobs {
				r, b, err := w.copyTable(gctx, t)
				if err != nil {
					return err
				}
				rows.Add(r)
				bytes.Add(b)
				c.logger.Info("table copied", "table", t.FullName(), "rows", r, "bytes", b,
					"progress", fmt.Sprintf("%d/%d", done.Add(1), len(tables)))
			}
			return nil
		})
	}
	err := g.Wait()
	stats := Stats{Tables: int(done.Load()), Rows: rows.Load(), Bytes: bytes.Load(), Duration: time.Since(start)}
	return stats, err
}

// worker owns one pair of connections.
type worker struct {
	c        *Copier
	snapshot string
	src, dst *pgx.Conn
}

// open connects whatever is not connected yet.
func (w *worker) open(ctx context.Context) error {
	if w.src == nil {
		conn, err := pgx.ConnectConfig(ctx, w.c.srcCfg)
		if err != nil {
			return fmt.Errorf("connect to s1: %w", err)
		}
		w.src = conn
	}
	if w.dst == nil {
		conn, err := pgx.ConnectConfig(ctx, w.c.dstCfg)
		if err != nil {
			return fmt.Errorf("connect to s3: %w", err)
		}
		// Triggers and foreign keys of the application must not interfere with the bulk load.
		if err := pg.SetReplicaRole(ctx, conn); err != nil {
			_ = conn.Close(context.WithoutCancel(ctx))
			return err
		}
		w.dst = conn
	}
	return nil
}

// reset drops both connections. Closing them also rolls back any open transaction, which is why a
// failed attempt needs no explicit cleanup.
func (w *worker) reset() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if w.src != nil {
		_ = w.src.Close(ctx)
		w.src = nil
	}
	if w.dst != nil {
		_ = w.dst.Close(ctx)
		w.dst = nil
	}
}

// copyTable copies one table, retrying transient failures, and records the outcome in table_progress.
func (w *worker) copyTable(ctx context.Context, t catalog.Table) (rows, bytes int64, err error) {
	w.status(ctx, t, meta.StatusRunning, 0, "")
	started := time.Now()
	w.c.logger.Info("copying table", "table", t.FullName(), "format", string(t.CopyFormat), "estimated_rows", t.EstimatedRows)

	for attempt := 1; ; attempt++ {
		rows, bytes, err = w.copyOnce(ctx, t, attempt > 1)
		if err == nil {
			break
		}
		w.reset()
		switch {
		case ctx.Err() != nil:
			// Another table failed and the group was cancelled: this table is simply not done.
			w.status(ctx, t, meta.StatusPending, -1, "interrupted because another table failed")
			return 0, 0, fmt.Errorf("copy %s: %w", t.FullName(), err)
		case attempt >= maxAttempts || !pg.IsRetryable(err):
			w.status(ctx, t, meta.StatusFailed, -1, err.Error())
			return 0, 0, fmt.Errorf("copy %s: %w", t.FullName(), err)
		}
		w.c.logger.Warn("copying a table failed, retrying", "table", t.FullName(), "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(attempt) * 2 * time.Second):
		}
	}

	w.status(ctx, t, meta.StatusDone, rows, "")
	w.c.logger.Debug("table done", "table", t.FullName(), "seconds", time.Since(started).Seconds())
	return rows, bytes, nil
}

// copyOnce makes one attempt: a REPEATABLE READ transaction on s1 that imports the exported snapshot,
// a transaction on s3, and the pipe between the two COPY commands. Nothing is visible on s3 unless
// the whole table arrived.
//
// With clean set (a retry) the table is truncated first. The previous attempt might have committed
// even though the client saw an error, and without primary keys yet on s3 a second load would
// silently duplicate the rows.
func (w *worker) copyOnce(ctx context.Context, t catalog.Table, clean bool) (rows, bytes int64, err error) {
	if err := w.open(ctx); err != nil {
		return 0, 0, err
	}
	out, in := copySQL(t)

	if _, err := w.src.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		return 0, 0, fmt.Errorf("begin on s1: %w", err)
	}
	// The name was validated against snapshotNamePattern, so it is safe to interpolate.
	if _, err := w.src.Exec(ctx, "SET TRANSACTION SNAPSHOT '"+w.snapshot+"'"); err != nil {
		return 0, 0, fmt.Errorf("import the exported snapshot on s1: %w", err)
	}
	if _, err := w.dst.Exec(ctx, "BEGIN"); err != nil {
		return 0, 0, fmt.Errorf("begin on s3: %w", err)
	}
	if clean {
		if _, err := w.dst.Exec(ctx, "TRUNCATE ONLY "+t.Quoted()); err != nil {
			return 0, 0, fmt.Errorf("truncate %s before the retry: %w", t.FullName(), err)
		}
	}

	pollCtx, stopPoll := context.WithCancel(ctx)
	defer stopPoll()
	go w.c.pollProgress(pollCtx, w.dst.PgConn().PID(), t)

	pr, pw := io.Pipe()
	counter := &countingWriter{w: pw}
	var outRows, inRows int64
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		tag, err := w.src.PgConn().CopyTo(gctx, counter, out)
		pw.CloseWithError(err) // a nil error ends the stream cleanly
		outRows = tag.RowsAffected()
		return err
	})
	g.Go(func() error {
		tag, err := w.dst.PgConn().CopyFrom(gctx, pr, in)
		pr.CloseWithError(err) // lets a blocked writer fail instead of waiting forever
		inRows = tag.RowsAffected()
		return err
	})
	if err := g.Wait(); err != nil {
		return 0, 0, err
	}
	if outRows != inRows {
		return 0, 0, fmt.Errorf("s1 sent %d rows but s3 stored %d", outRows, inRows)
	}
	if _, err := w.dst.Exec(ctx, "COMMIT"); err != nil {
		return 0, 0, fmt.Errorf("commit on s3: %w", err)
	}
	if _, err := w.src.Exec(ctx, "COMMIT"); err != nil {
		return 0, 0, fmt.Errorf("end the read transaction on s1: %w", err)
	}
	return inRows, counter.n.Load(), nil
}

// pollProgress writes the row counter of the running COPY to table_progress until ctx ends. The
// counter comes from pg_stat_progress_copy of the s3 backend, so it costs nothing on the data path.
func (c *Copier) pollProgress(ctx context.Context, pid uint32, t catalog.Table) {
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var n int64
			err := c.dst.QueryRow(ctx,
				`SELECT COALESCE(sum(tuples_processed), 0)::bigint FROM pg_stat_progress_copy WHERE pid = $1`, int32(pid)).Scan(&n)
			if err != nil || n == 0 {
				continue
			}
			// The counter is only for 'gophant status'; a failed write must not disturb the copy.
			if err := c.store.SetRowsCopied(ctx, c.dst, t.Schema, t.Name, n); err != nil && ctx.Err() == nil {
				c.logger.Debug("could not record the progress of a table", "table", t.FullName(), "error", err)
			}
		}
	}
}

// status records the state of a table. It uses a context that survives cancellation so that a
// failure is still written down.
func (w *worker) status(ctx context.Context, t catalog.Table, status string, rows int64, errMsg string) {
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusTimeout)
	defer cancel()
	if err := w.c.store.SetSnapshotStatus(bg, w.c.dst, t.Schema, t.Name, status, rows, errMsg); err != nil {
		w.c.logger.Warn("could not record the table status", "table", t.FullName(), "status", status, "error", err)
	}
}

// countingWriter counts the bytes that pass through it.
type countingWriter struct {
	w io.Writer
	n atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}
