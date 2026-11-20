// Package snapshot creates the logical replication slot, copies a consistent snapshot of every table
// with parallel streaming COPY, and orchestrates the snapshot stage.
package snapshot

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shinession/gophant/internal/pg"
)

// outputPlugin is the logical decoding plugin that the replication slot uses.
const outputPlugin = "pgoutput"

// snapshotNamePattern is the shape of an exported snapshot identifier, for example 00000003-0000001B-1.
// The name is interpolated into SET TRANSACTION SNAPSHOT, which cannot take a parameter, so it is
// validated first.
var snapshotNamePattern = regexp.MustCompile(`^[0-9A-Fa-f]+(-[0-9A-Fa-f]+)+$`)

// Slot is a logical replication slot together with the snapshot that was exported when it was
// created. The snapshot is exactly the state of the database at ConsistentLSN, so rows copied under
// it plus the WAL stream from ConsistentLSN on reproduce the source without gap or overlap.
//
// The snapshot lives only as long as the replication connection stays open and idle. Sending any
// command on it, even a harmless one, invalidates the snapshot; Close releases it.
type Slot struct {
	Name          string
	ConsistentLSN pglogrepl.LSN
	SnapshotName  string

	conn   *pgconn.PgConn
	logger *slog.Logger
}

// CreateSlot creates the slot with CREATE_REPLICATION_SLOT ... LOGICAL pgoutput EXPORT_SNAPSHOT.
//
// The command must not run inside a transaction block (PostgreSQL rejects EXPORT_SNAPSHOT there);
// it has to be the first command of a fresh replication connection. The call can take a while:
// the slot only becomes consistent when every transaction that was running at that moment ended.
func CreateSlot(ctx context.Context, replicationDSN, name string, logger *slog.Logger) (*Slot, error) {
	conn, err := pg.ConnectReplication(ctx, replicationDSN, "gophant-snapshot")
	if err != nil {
		return nil, err
	}

	stopLog := logWhileWaiting(logger, "waiting for the replication slot to become consistent (running transactions have to finish first)")
	res, err := pglogrepl.CreateReplicationSlot(ctx, conn, name, outputPlugin, pglogrepl.CreateReplicationSlotOptions{
		Mode:           pglogrepl.LogicalReplication,
		SnapshotAction: "EXPORT_SNAPSHOT",
	})
	stopLog()
	if err != nil {
		closeQuietly(conn)
		return nil, fmt.Errorf("create replication slot %s: %w", name, err)
	}

	lsn, err := pglogrepl.ParseLSN(res.ConsistentPoint)
	if err != nil {
		closeQuietly(conn)
		return nil, fmt.Errorf("slot %s returned an invalid consistent point: %w", name, err)
	}
	if !snapshotNamePattern.MatchString(res.SnapshotName) {
		closeQuietly(conn)
		return nil, fmt.Errorf("slot %s returned an unexpected snapshot name %q", name, res.SnapshotName)
	}
	logger.Info("replication slot created", "slot", res.SlotName, "consistent_lsn", lsn.String(), "snapshot", res.SnapshotName)
	return &Slot{Name: res.SlotName, ConsistentLSN: lsn, SnapshotName: res.SnapshotName, conn: conn, logger: logger}, nil
}

// Close ends the replication connection and with it the exported snapshot. The slot itself is
// persistent and keeps holding back WAL on s1 until it is consumed or dropped.
func (s *Slot) Close(ctx context.Context) error {
	if s == nil || s.conn == nil {
		return nil
	}
	conn := s.conn
	s.conn = nil
	if err := conn.Close(ctx); err != nil {
		return fmt.Errorf("close the snapshot connection: %w", err)
	}
	s.logger.Info("snapshot released", "slot", s.Name)
	return nil
}

// DropSlot removes the slot from s1. A slot that does not exist is not an error. A slot that is
// still held by a connection is retried for a short while, because the server needs a moment to
// notice that a connection that was just closed is gone.
func DropSlot(ctx context.Context, q pg.Querier, name string, logger *slog.Logger) error {
	const attempts = 10
	var lastErr error
	for i := 0; i < attempts; i++ {
		var exists bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = $1)`, name).Scan(&exists); err != nil {
			return fmt.Errorf("look up slot %s: %w", name, err)
		}
		if !exists {
			return nil
		}
		_, err := q.Exec(ctx, `SELECT pg_drop_replication_slot($1)`, name)
		if err == nil {
			logger.Info("replication slot dropped", "slot", name)
			return nil
		}
		lastErr = err
		if pg.SQLState(err) != "55006" { // object_in_use
			return fmt.Errorf("drop slot %s: %w", name, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("drop slot %s: it is still in use by another connection (is gophant sync or snapshot running?): %w", name, lastErr)
}

// logWhileWaiting logs msg every 30 seconds until the returned function is called.
func logWhileWaiting(logger *slog.Logger, msg string) (stop func()) {
	done := make(chan struct{})
	go func() {
		start := time.Now()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				logger.Info(msg, "waited", time.Since(start).Round(time.Second).String())
			}
		}
	}()
	return func() { close(done) }
}

func closeQuietly(conn *pgconn.PgConn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}
