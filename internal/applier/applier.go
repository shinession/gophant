package applier

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shinession/gophant/internal/cdc"
	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/meta"
	"github.com/shinession/gophant/internal/pg"
)

const (
	// maxBatchStatements bounds how many statements go to s3 in one round trip.
	maxBatchStatements = 1000
	// maxBatchChanges bounds the changes merged into one s3 transaction.
	maxBatchChanges = 20000
)

// textCompareTypes are the types that cannot be compared with = (or compare differently from what the
// row contains), so rows are located by the text form of these columns. The array forms start with _.
var textCompareTypes = []string{
	"json", "_json", "jsonpath", "_jsonpath", "xml", "_xml",
	"point", "_point", "line", "_line", "lseg", "_lseg", "box", "_box", "path", "_path", "polygon", "_polygon", "circle", "_circle",
	"pg_snapshot", "_pg_snapshot", "txid_snapshot", "_txid_snapshot",
}

// Stats counts what the applier did.
type Stats struct {
	Txs            atomic.Int64
	Batches        atomic.Int64
	Inserts        atomic.Int64
	Updates        atomic.Int64
	Deletes        atomic.Int64
	Truncates      atomic.Int64
	MissingDeletes atomic.Int64
}

// Applier replays transactions on s3. One goroutine drives it; the connection is not shared.
type Applier struct {
	cfg      *config.Config
	store    *meta.Store
	dstCfg   *pgx.ConnConfig
	progress *cdc.Progress
	logger   *slog.Logger

	conn   *pgx.Conn
	tables map[string]*TableMeta

	// Stats can be read from other goroutines.
	Stats Stats
}

// New prepares an applier. The connection is opened by Run.
func New(cfg *config.Config, progress *cdc.Progress, logger *slog.Logger) (*Applier, error) {
	dstCfg, err := pg.ParseConfig(cfg.Target.DSN, "gophant-apply")
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	return &Applier{
		cfg: cfg, store: meta.NewStore(cfg.Target.MetaSchema), dstCfg: dstCfg,
		progress: progress, logger: logger, tables: make(map[string]*TableMeta),
	}, nil
}

// Run applies the events until the channel is closed or a statement fails. Transactions are merged
// into one s3 transaction up to cdc.batch_max_txns or until cdc.batch_max_delay has passed since the
// first one; a transaction that was spilled to disk is applied on its own.
//
// Every transaction is released (Done) when it is applied. After an error the transactions that are
// still held by the applier are released as well; the caller drains the channel.
func (a *Applier) Run(ctx context.Context, events <-chan cdc.Event) error {
	if err := a.connect(ctx); err != nil {
		return err
	}
	defer a.close()

	var (
		batch        []*cdc.Tx
		batchChanges int
		timer        *time.Timer
		timerC       <-chan time.Time
	)
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timer, timerC = nil, nil
		}
	}
	defer func() {
		stopTimer()
		for _, t := range batch {
			t.Done()
		}
	}()
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		stopTimer()
		if err := a.applyBatch(ctx, batch); err != nil {
			return err
		}
		batch, batchChanges = nil, 0
		return nil
	}

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return flush()
			}
			if ev.Tx == nil {
				// An idle position can only arrive when nothing is in flight, so there is no batch to wait for.
				if err := flush(); err != nil {
					return err
				}
				if err := a.advanceIdle(ctx, ev.Idle); err != nil {
					return err
				}
				continue
			}
			if len(batch) == 0 {
				timer = time.NewTimer(a.cfg.CDC.BatchMaxDelay.Std())
				timerC = timer.C
			}
			batch = append(batch, ev.Tx)
			batchChanges += ev.Tx.Changes.Len()
			if len(batch) >= a.cfg.CDC.BatchMaxTxns || batchChanges >= maxBatchChanges || ev.Tx.Changes.Spilled() {
				if err := flush(); err != nil {
					return err
				}
			}
		case <-timerC:
			if err := flush(); err != nil {
				return err
			}
		}
	}
}

func (a *Applier) connect(ctx context.Context) error {
	conn, err := pgx.ConnectConfig(ctx, a.dstCfg)
	if err != nil {
		return fmt.Errorf("connect to s3: %w", err)
	}
	// Business triggers must not fire a second time and foreign keys must not interfere: s1 already
	// executed both.
	if err := pg.SetReplicaRole(ctx, conn); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		return err
	}
	// Once s1 is told that a position is flushed it may delete the WAL before it, so the commit on s3
	// has to be durable first, whatever the default of the server is.
	if _, err := conn.Exec(ctx, "SET synchronous_commit = 'on'"); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		return fmt.Errorf("set synchronous_commit: %w", err)
	}
	a.conn = conn
	return nil
}

func (a *Applier) close() {
	if a.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = a.conn.Close(ctx)
	a.conn = nil
}

// advanceIdle records an idle stream position as applied.
func (a *Applier) advanceIdle(ctx context.Context, lsn pglogrepl.LSN) error {
	if _, err := a.store.AdvanceIdle(ctx, a.conn, lsn); err != nil {
		return err
	}
	a.progress.AdvanceFlushed(lsn)
	return nil
}

// applyBatch replays the transactions in one s3 transaction. The statement that moves applied_lsn to
// the end of the last transaction is part of it: the business rows and the position are committed
// together or not at all, which is what makes the replay exactly-once.
func (a *Applier) applyBatch(ctx context.Context, txs []*cdc.Tx) (err error) {
	first, last := txs[0], txs[len(txs)-1]
	started := time.Now()
	defer func() {
		if err != nil {
			err = fmt.Errorf("replay of the transactions committed at %s..%s failed: %w", first.CommitLSN, last.CommitLSN, err)
		}
	}()

	tx, err := a.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin on s3: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // a no-op after Commit

	w := &batchWriter{tx: tx, batch: &pgx.Batch{}, applier: a}
	var changes int
	for _, t := range txs {
		if err := t.Changes.Iterate(func(c cdc.Change) error {
			changes++
			return a.queueChange(ctx, tx, w, c)
		}); err != nil {
			return err
		}
	}
	if err := w.add(ctx, Stmt{
		SQL: a.store.AppliedLSNStatement(), Args: []any{last.EndLSN.String()}, Kind: kindPosition,
		Desc: "UPDATE applied_lsn to " + last.EndLSN.String(),
	}); err != nil {
		return err
	}
	if err := w.flush(ctx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit on s3: %w", err)
	}

	// Only now, after the commit, is the position durable on s3 and may be reported to s1.
	a.progress.AdvanceFlushed(last.EndLSN)
	for _, t := range txs {
		t.Done()
	}
	a.Stats.Txs.Add(int64(len(txs)))
	a.Stats.Batches.Add(1)
	a.logger.Debug("batch applied", "transactions", len(txs), "changes", changes, "end_lsn", last.EndLSN.String(),
		"millis", time.Since(started).Milliseconds())
	return nil
}

// queueChange builds the statement of one change and adds it to the batch.
func (a *Applier) queueChange(ctx context.Context, q pg.Querier, w *batchWriter, c cdc.Change) error {
	var (
		st   Stmt
		skip bool
		err  error
	)
	switch c.Op {
	case cdc.OpInsert:
		var tm *TableMeta
		if tm, err = a.tableMeta(ctx, q, c.Rel); err == nil {
			st, err = BuildInsert(c.Rel, tm, c.New)
		}
	case cdc.OpUpdate:
		var tm *TableMeta
		if tm, err = a.tableMeta(ctx, q, c.Rel); err == nil {
			st, skip, err = BuildUpdate(c.Rel, tm, c.OldKind, c.Old, c.New)
		}
	case cdc.OpDelete:
		var tm *TableMeta
		if tm, err = a.tableMeta(ctx, q, c.Rel); err == nil {
			st, err = BuildDelete(c.Rel, tm, c.Old)
		}
	case cdc.OpTruncate:
		st = BuildTruncate(c.Rels, c.TruncOpt)
	default:
		err = fmt.Errorf("unknown change type %q", string(c.Op))
	}
	if err != nil {
		return err
	}
	if skip {
		return nil
	}
	return w.add(ctx, st)
}

// kindPosition is the internal kind of the applied_lsn statement.
const kindPosition Kind = 100

// batchWriter queues statements in a pgx.Batch and checks every result.
type batchWriter struct {
	tx      pgx.Tx
	batch   *pgx.Batch
	pending []pendingStmt
	applier *Applier
}

type pendingStmt struct {
	kind Kind
	desc string
}

func (w *batchWriter) add(ctx context.Context, st Stmt) error {
	w.batch.Queue(st.SQL, st.Args...)
	w.pending = append(w.pending, pendingStmt{kind: st.Kind, desc: st.Desc})
	if w.batch.Len() >= maxBatchStatements {
		return w.flush(ctx)
	}
	return nil
}

// flush sends the queued statements and verifies their effect: an INSERT stores one row, an UPDATE
// finds exactly one row, a DELETE removes at most one and the position update moves the position.
func (w *batchWriter) flush(ctx context.Context) error {
	if w.batch.Len() == 0 {
		return nil
	}
	results := w.tx.SendBatch(ctx, w.batch)
	pending := w.pending
	w.batch, w.pending = &pgx.Batch{}, nil

	for _, p := range pending {
		tag, err := results.Exec()
		if err != nil {
			_ = results.Close()
			return fmt.Errorf("%s: %w", p.desc, describe(err))
		}
		if err := w.applier.check(p, tag); err != nil {
			_ = results.Close()
			return err
		}
	}
	if err := results.Close(); err != nil {
		return fmt.Errorf("close the batch: %w", err)
	}
	return nil
}

func (a *Applier) check(p pendingStmt, tag pgconn.CommandTag) error {
	n := tag.RowsAffected()
	switch p.kind {
	case KindInsert:
		if n != 1 {
			return fmt.Errorf("%s stored %d rows, expected 1", p.desc, n)
		}
		a.Stats.Inserts.Add(1)
	case KindUpdate:
		if n != 1 {
			return fmt.Errorf("%s changed %d rows, expected 1: the row is missing or duplicated on s3, the two databases have diverged", p.desc, n)
		}
		a.Stats.Updates.Add(1)
	case KindDelete:
		switch {
		case n > 1:
			return fmt.Errorf("%s removed %d rows, expected at most 1", p.desc, n)
		case n == 0:
			// The wanted end state (the row is gone) holds, but the row should have been there.
			a.Stats.MissingDeletes.Add(1)
			a.logger.Warn("a DELETE found no row on s3", "operation", p.desc)
		}
		a.Stats.Deletes.Add(1)
	case KindTruncate:
		a.Stats.Truncates.Add(1)
	case kindPosition:
		if n != 1 {
			return meta.ErrAppliedLSNNotAdvanced
		}
	}
	return nil
}

// describe adds the position of a server error, which names the offending constraint or column.
func describe(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName != "" {
		return fmt.Errorf("%w (constraint %s)", err, pgErr.ConstraintName)
	}
	return err
}

const tableKindSQL = `
SELECT c.relkind::text,
       EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisprimary)
FROM pg_class c
WHERE c.oid = to_regclass($1)`

const tableColumnsSQL = `
SELECT a.attname,
       a.attgenerated::text,
       a.attidentity::text,
       COALESCE(bt.typname, t.typname) = ANY ($2::text[])
FROM pg_attribute a
JOIN pg_type t ON t.oid = a.atttypid
LEFT JOIN pg_type bt ON bt.oid = t.typbasetype AND t.typtype = 'd'
WHERE a.attrelid = to_regclass($1) AND a.attnum > 0 AND NOT a.attisdropped`

// tableMeta returns what s3 knows about a table, read once per table.
func (a *Applier) tableMeta(ctx context.Context, q pg.Querier, rel *cdc.Relation) (*TableMeta, error) {
	key := rel.FullName()
	if tm, ok := a.tables[key]; ok {
		return tm, nil
	}
	name := pg.Qualified(rel.Schema, rel.Name)

	tm := &TableMeta{Columns: make(map[string]ColumnMeta)}
	var kind string
	err := q.QueryRow(ctx, tableKindSQL, name).Scan(&kind, &tm.HasPK)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("table %s of the change stream does not exist on s3", key)
	}
	if err != nil {
		return nil, fmt.Errorf("look up %s on s3: %w", key, err)
	}
	tm.Partitioned = kind == "p"

	rows, err := q.Query(ctx, tableColumnsSQL, name, textCompareTypes)
	if err != nil {
		return nil, fmt.Errorf("read the columns of %s on s3: %w", key, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			col              string
			generated, ident string
			cm               ColumnMeta
		)
		if err := rows.Scan(&col, &generated, &ident, &cm.TextCompare); err != nil {
			return nil, fmt.Errorf("read the columns of %s on s3: %w", key, err)
		}
		cm.Generated = generated != ""
		cm.IdentityAlways = ident == "a"
		tm.Columns[col] = cm
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the columns of %s on s3: %w", key, err)
	}
	a.tables[key] = tm
	return tm, nil
}
