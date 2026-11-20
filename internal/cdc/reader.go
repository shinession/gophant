package cdc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/pg"
	"golang.org/x/sync/semaphore"
)

const (
	// drainTimeout bounds how long the reader waits for the applier to finish its queue at shutdown.
	drainTimeout = 2 * time.Minute
	// idleInterval limits how often an idle position is handed to the applier.
	idleInterval = time.Second
	// minBudget is the smallest memory budget of queued transactions.
	minBudget = 32 << 20
	// txOverhead is added to the memory weight of every queued transaction.
	txOverhead = 4096
)

// errStreamEnded is returned when s1 ends the stream, for example during a shutdown of the server.
var errStreamEnded = errors.New("the server ended the replication stream")

// Event is what the reader hands to the applier: a committed transaction, or the position up to which
// an idle stream has been decoded.
type Event struct {
	Tx *Tx
	// Idle is set (non-zero) when nothing is in flight and s1 has decoded the WAL up to this position
	// without sending anything for it. The applier records it as applied, which lets the lag of an
	// idle migration reach zero.
	Idle pglogrepl.LSN
}

// Reader consumes the pgoutput stream of the replication slot.
type Reader struct {
	cfg      *config.Config
	progress *Progress
	logger   *slog.Logger
	budget   *semaphore.Weighted
	budgetN  int64
}

// NewReader returns a reader. The memory held by transactions that wait for the applier is bounded
// to about twice cdc.max_tx_memory_mb: a slow s3 slows the reader down instead of exhausting memory.
func NewReader(cfg *config.Config, progress *Progress, logger *slog.Logger) *Reader {
	n := max(2*cfg.MaxTxMemoryBytes(), minBudget)
	return &Reader{cfg: cfg, progress: progress, logger: logger, budget: semaphore.NewWeighted(n), budgetN: n}
}

// Run streams from start and sends events to out until ctx ends or an error occurs. A transaction
// is delivered only if it committed at or after start, which skips what was already applied.
//
// Run owns out and closes it when the stream ends. It then waits for applierDone, so that the final
// position of the applier can still be reported to s1, and returns the reason the stream ended: nil
// when ctx was cancelled.
func (r *Reader) Run(ctx context.Context, start pglogrepl.LSN, out chan<- Event, applierDone <-chan struct{}) error {
	conn, err := pg.ConnectReplication(ctx, pg.ReplicationDSN(r.cfg.Source.DSN, r.cfg.Source.ReplicationDSN), "gophant-sync")
	if err != nil {
		close(out)
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	err = pglogrepl.StartReplication(ctx, conn, r.cfg.Source.SlotName, start, pglogrepl.StartReplicationOptions{
		Mode: pglogrepl.LogicalReplication,
		PluginArgs: []string{
			"proto_version '1'",
			fmt.Sprintf("publication_names '%s'", r.cfg.Source.PublicationName),
		},
	})
	if err != nil {
		close(out)
		return fmt.Errorf("start replication on slot %s from %s: %w", r.cfg.Source.SlotName, start, err)
	}
	r.logger.Info("streaming started", "slot", r.cfg.Source.SlotName, "publication", r.cfg.Source.PublicationName, "start_lsn", start.String())

	// The heartbeat must keep the connection alive until the applier has drained its queue, so it is
	// not tied to ctx, which a shutdown signal cancels.
	hb := &heartbeat{conn: conn, progress: r.progress, interval: r.cfg.CDC.StandbyStatusInterval.Std(), kick: make(chan struct{}, 1), logger: r.logger}
	hbCtx, stopHeartbeat := context.WithCancel(context.WithoutCancel(ctx))
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		hb.run(hbCtx)
	}()

	loopErr := r.receiveLoop(ctx, conn, start, out, hb)
	close(out)

	select {
	case <-applierDone:
	case <-time.After(drainTimeout):
		r.logger.Warn("the applier did not finish its queue in time, the final position is not reported")
	}
	stopHeartbeat()
	<-hbDone
	// Best effort: tell s1 how far s3 got, so that it can release the WAL before that position.
	finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hb.send(finalCtx)
	return loopErr
}

// receiveLoop reads messages, assembles transactions and sends them to out.
func (r *Reader) receiveLoop(ctx context.Context, conn *pgconn.PgConn, start pglogrepl.LSN, out chan<- Event, hb *heartbeat) (err error) {
	dec := NewDecoder(&r.cfg.Filter)
	// position is the exclusive end of everything that was delivered or applied before: a
	// transaction whose commit record starts below it has been applied already.
	position := start
	var cur *Tx
	defer func() {
		if cur != nil {
			cur.Done()
		}
	}()
	var lastIdle time.Time

	for {
		raw, err := conn.ReceiveMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("receive from the replication stream: %w", err)
		}

		switch m := raw.(type) {
		case *pgproto3.ErrorResponse:
			return pgconn.ErrorResponseToPgError(m)
		case *pgproto3.CopyDone:
			return errStreamEnded
		case *pgproto3.CopyData:
			if len(m.Data) == 0 {
				continue
			}
			switch m.Data[0] {
			case pglogrepl.PrimaryKeepaliveMessageByteID:
				pkm, err := pglogrepl.ParsePrimaryKeepaliveMessage(m.Data[1:])
				if err != nil {
					return err
				}
				r.progress.AdvanceWritten(pkm.ServerWALEnd)
				if pkm.ReplyRequested {
					hb.Kick()
				}
				// Idle advance: with no transaction open, none queued and none in the applier, every
				// transaction that committed before ServerWALEnd has been delivered already (the server
				// sends them before the keepalive), so the position can move to ServerWALEnd.
				if cur == nil && r.progress.InFlight() == 0 && pkm.ServerWALEnd > r.progress.Flushed() &&
					time.Since(lastIdle) >= idleInterval {
					select {
					case out <- Event{Idle: pkm.ServerWALEnd}:
						lastIdle = time.Now()
					case <-ctx.Done():
						return nil
					}
				}

			case pglogrepl.XLogDataByteID:
				xld, err := pglogrepl.ParseXLogData(m.Data[1:])
				if err != nil {
					return err
				}
				r.progress.AdvanceWritten(xld.WALStart + pglogrepl.LSN(len(xld.WALData)))
				msg, err := dec.Decode(xld.WALData)
				if err != nil {
					return err
				}
				switch msg.Kind {
				case MsgBegin:
					if cur != nil {
						return errors.New("protocol error: Begin inside a transaction")
					}
					cur = &Tx{
						Xid:     msg.Begin.Xid,
						Changes: NewChangeBuffer(r.cfg.MaxTxMemoryBytes(), r.cfg.CDC.TempDir),
						logger:  r.logger,
					}
				case MsgChange:
					if cur == nil {
						return errors.New("protocol error: a change outside a transaction")
					}
					if err := cur.Changes.Add(msg.Change); err != nil {
						return err
					}
				case MsgCommit:
					if cur == nil {
						return errors.New("protocol error: Commit without Begin")
					}
					tx := cur
					cur = nil
					tx.CommitLSN, tx.EndLSN, tx.CommitTime = msg.Commit.CommitLSN, msg.Commit.EndLSN, msg.Commit.CommitTime
					if tx.CommitLSN < position {
						r.logger.Debug("skipping an already applied transaction", "xid", tx.Xid, "commit_lsn", tx.CommitLSN.String())
						tx.Done()
						continue
					}
					position = tx.EndLSN
					if err := r.deliver(ctx, tx, out); err != nil {
						return err
					}
				}
			}
		}
	}
}

// deliver hands a transaction to the applier once the memory budget allows it.
func (r *Reader) deliver(ctx context.Context, tx *Tx, out chan<- Event) error {
	weight := min(tx.Changes.MemBytes()+txOverhead, r.budgetN)
	if err := r.budget.Acquire(ctx, weight); err != nil {
		tx.Done()
		return nil // the context ended
	}
	r.progress.inflight.Add(1)
	tx.onDone = func() {
		r.budget.Release(weight)
		r.progress.inflight.Add(-1)
	}
	select {
	case out <- Event{Tx: tx}:
		return nil
	case <-ctx.Done():
		tx.Done()
		return nil
	}
}

// heartbeat reports the position to s1 on a timer. It runs in its own goroutine so that a busy
// applier, or a reader waiting for the memory budget, never makes s1 time the connection out.
type heartbeat struct {
	conn     *pgconn.PgConn
	progress *Progress
	interval time.Duration
	kick     chan struct{}
	logger   *slog.Logger
}

// Kick asks for an immediate report, which s1 requests with a keepalive that wants a reply.
func (h *heartbeat) Kick() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

func (h *heartbeat) run(ctx context.Context) {
	timer := time.NewTimer(h.next())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.kick:
		case <-timer.C:
		}
		if err := h.send(ctx); err != nil && ctx.Err() == nil {
			h.logger.Warn("could not send a standby status update", "error", err)
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(h.next())
	}
}

func (h *heartbeat) next() time.Duration {
	if h.progress.Fast() {
		return min(h.interval, time.Second)
	}
	return h.interval
}

// send reports the flushed position. ReplyRequested makes s1 answer with a keepalive that carries
// the position it has decoded, which is what lets an idle stream move forward.
//
// The flush position must never be 0: pglogrepl replaces a zero flush position by the write position,
// which would tell s1 that everything received is already durable on s3.
func (h *heartbeat) send(ctx context.Context) error {
	flushed := h.progress.Flushed()
	if flushed == 0 {
		return nil
	}
	written := max(h.progress.Written(), flushed)
	return pglogrepl.SendStandbyStatusUpdate(ctx, h.conn, pglogrepl.StandbyStatusUpdate{
		WALWritePosition: written,
		WALFlushPosition: flushed,
		WALApplyPosition: flushed,
		ReplyRequested:   true,
	})
}
