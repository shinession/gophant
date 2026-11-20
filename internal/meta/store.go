package meta

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/shinession/gophant/internal/pg"
)

// SyncState is the single row of sync_state.
type SyncState struct {
	Stage         Stage
	SlotName      string
	Publication   string
	ConsistentLSN *pglogrepl.LSN
	AppliedLSN    *pglogrepl.LSN
	UpdatedAt     time.Time
}

// StartLSN returns the position from which the WAL stream must be consumed: the applied position,
// or the consistent point of the snapshot when nothing has been applied yet.
func (s *SyncState) StartLSN() (pglogrepl.LSN, bool) {
	switch {
	case s.AppliedLSN != nil:
		return *s.AppliedLSN, true
	case s.ConsistentLSN != nil:
		return *s.ConsistentLSN, true
	}
	return 0, false
}

// TableProgress is one row of table_progress.
type TableProgress struct {
	Schema         string
	Name           string
	HasPK          bool
	ReplicaID      string
	SnapshotStatus string
	RowsCopied     int64
	VerifiedOK     *bool
	ErrorMsg       string
	UpdatedAt      time.Time
}

// FullName returns schema.name without quoting, for messages.
func (t TableProgress) FullName() string { return t.Schema + "." + t.Name }

// ErrAppliedLSNNotAdvanced is returned when the guarded applied_lsn update touched no row. Inside
// the replay transaction this means the stored position is not behind the batch, i.e. the batch
// would be applied a second time; the transaction must be rolled back.
var ErrAppliedLSNNotAdvanced = errors.New("applied_lsn did not advance: the stored position is already at or beyond this batch")

// Store reads and writes the meta tables of one schema. Every method accepts a pg.Querier, so the
// caller decides whether it runs on a pool, a connection or inside a pgx.Tx (the replay engine
// updates applied_lsn in the same transaction as the business rows).
type Store struct {
	schema string
}

// NewStore returns a store for the given meta schema.
func NewStore(schema string) *Store { return &Store{schema: schema} }

// Schema returns the meta schema name.
func (s *Store) Schema() string { return s.schema }

func (s *Store) state() string    { return pg.Qualified(s.schema, "sync_state") }
func (s *Store) progress() string { return pg.Qualified(s.schema, "table_progress") }

// Ensure creates the meta schema and tables if needed.
func (s *Store) Ensure(ctx context.Context, q pg.Querier) error { return Ensure(ctx, q, s.schema) }

// Exists reports whether the sync_state table exists.
func (s *Store) Exists(ctx context.Context, q pg.Querier) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, s.state()).Scan(&exists)
	return exists, err
}

// GetState returns the state row, or nil when it does not exist (or the meta tables are missing).
func (s *Store) GetState(ctx context.Context, q pg.Querier) (*SyncState, error) {
	exists, err := s.Exists(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("check %s: %w", s.state(), err)
	}
	if !exists {
		return nil, nil
	}
	var (
		st                          SyncState
		stage                       string
		consistentText, appliedText *string
	)
	err = q.QueryRow(ctx, fmt.Sprintf(
		`SELECT stage, slot_name, publication, consistent_lsn::text, applied_lsn::text, updated_at FROM %s WHERE id = 1`,
		s.state())).Scan(&stage, &st.SlotName, &st.Publication, &consistentText, &appliedText, &st.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.state(), err)
	}
	st.Stage = Stage(stage)
	if st.ConsistentLSN, err = parseLSNPtr(consistentText); err != nil {
		return nil, err
	}
	if st.AppliedLSN, err = parseLSNPtr(appliedText); err != nil {
		return nil, err
	}
	return &st, nil
}

// InitState inserts the state row in the PRECHECK stage. It does nothing when the row exists.
func (s *Store) InitState(ctx context.Context, q pg.Querier, slot, publication string) error {
	_, err := q.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s (id, stage, slot_name, publication) VALUES (1, $1, $2, $3) ON CONFLICT (id) DO NOTHING`, s.state()),
		string(StagePrecheck), slot, publication)
	if err != nil {
		return fmt.Errorf("initialize %s: %w", s.state(), err)
	}
	return nil
}

// ResetState removes the state row and all table progress rows (used when a snapshot is restarted).
func (s *Store) ResetState(ctx context.Context, q pg.Querier) error {
	for _, stmt := range []string{
		fmt.Sprintf(`DELETE FROM %s`, s.state()),
		fmt.Sprintf(`DELETE FROM %s`, s.progress()),
	} {
		if _, err := q.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("reset meta state: %w", err)
		}
	}
	return nil
}

// SetStage updates the stage.
func (s *Store) SetStage(ctx context.Context, q pg.Querier, stage Stage) error {
	if !stage.Valid() {
		return fmt.Errorf("invalid stage %q", stage)
	}
	tag, err := q.Exec(ctx, fmt.Sprintf(`UPDATE %s SET stage = $1, updated_at = now() WHERE id = 1`, s.state()), string(stage))
	if err != nil {
		return fmt.Errorf("set stage %s: %w", stage, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("set stage %s: %s has no state row", stage, s.state())
	}
	return nil
}

// SetSnapshotPoint records the consistent point returned by the slot creation and moves to SNAPSHOT.
func (s *Store) SetSnapshotPoint(ctx context.Context, q pg.Querier, consistent pglogrepl.LSN) error {
	tag, err := q.Exec(ctx, fmt.Sprintf(
		`UPDATE %s SET stage = $1, consistent_lsn = $2::pg_lsn, applied_lsn = NULL, updated_at = now() WHERE id = 1`, s.state()),
		string(StageSnapshot), consistent.String())
	if err != nil {
		return fmt.Errorf("record consistent_lsn: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("record consistent_lsn: %s has no state row", s.state())
	}
	return nil
}

// AppliedLSNStatement is the guarded statement that advances applied_lsn. It matches no row when
// the stored position is not behind $1, which the replay engine treats as a duplicate application.
// The only argument is the new position as pg_lsn text.
func (s *Store) AppliedLSNStatement() string {
	return fmt.Sprintf(
		`UPDATE %s SET applied_lsn = $1::pg_lsn, updated_at = now() WHERE id = 1 AND (applied_lsn IS NULL OR applied_lsn < $1::pg_lsn)`,
		s.state())
}

// SetAppliedLSN advances applied_lsn to lsn and returns ErrAppliedLSNNotAdvanced when it did not
// move. Call it with a pgx.Tx to make the update atomic with business data.
func (s *Store) SetAppliedLSN(ctx context.Context, q pg.Querier, lsn pglogrepl.LSN) error {
	tag, err := q.Exec(ctx, s.AppliedLSNStatement(), lsn.String())
	if err != nil {
		return fmt.Errorf("advance applied_lsn to %s: %w", lsn, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("advance applied_lsn to %s: %w", lsn, ErrAppliedLSNNotAdvanced)
	}
	return nil
}

// AdvanceIdle moves applied_lsn forward when the stream is idle and reports whether it moved. A
// position that is not ahead of the stored one is not an error here.
func (s *Store) AdvanceIdle(ctx context.Context, q pg.Querier, lsn pglogrepl.LSN) (bool, error) {
	tag, err := q.Exec(ctx, s.AppliedLSNStatement(), lsn.String())
	if err != nil {
		return false, fmt.Errorf("advance idle applied_lsn to %s: %w", lsn, err)
	}
	return tag.RowsAffected() == 1, nil
}

// UpsertTables inserts table rows in the PENDING state. An existing row keeps its status and counters
// and only refreshes the structural attributes.
func (s *Store) UpsertTables(ctx context.Context, q pg.Querier, tables []TableProgress) error {
	stmt := fmt.Sprintf(`INSERT INTO %s (schema_name, table_name, has_pk, replica_id) VALUES ($1, $2, $3, $4)
ON CONFLICT (schema_name, table_name) DO UPDATE SET has_pk = EXCLUDED.has_pk, replica_id = EXCLUDED.replica_id, updated_at = now()`, s.progress())
	for _, t := range tables {
		rid := t.ReplicaID
		if rid == "" {
			rid = "d"
		}
		if _, err := q.Exec(ctx, stmt, t.Schema, t.Name, t.HasPK, rid); err != nil {
			return fmt.Errorf("register table %s: %w", t.FullName(), err)
		}
	}
	return nil
}

// SetSnapshotStatus records the copy status of one table. rows is stored only when it is >= 0.
func (s *Store) SetSnapshotStatus(ctx context.Context, q pg.Querier, schema, name, status string, rows int64, errMsg string) error {
	_, err := q.Exec(ctx, fmt.Sprintf(`UPDATE %s SET snapshot_status = $3,
    rows_copied = CASE WHEN $4::bigint >= 0 THEN $4::bigint ELSE rows_copied END,
    error_msg = NULLIF($5, ''), updated_at = now()
WHERE schema_name = $1 AND table_name = $2`, s.progress()), schema, name, status, rows, errMsg)
	if err != nil {
		return fmt.Errorf("record status of %s.%s: %w", schema, name, err)
	}
	return nil
}

// SetRowsCopied updates the running row counter of one table.
func (s *Store) SetRowsCopied(ctx context.Context, q pg.Querier, schema, name string, rows int64) error {
	_, err := q.Exec(ctx, fmt.Sprintf(
		`UPDATE %s SET rows_copied = $3, updated_at = now() WHERE schema_name = $1 AND table_name = $2`, s.progress()),
		schema, name, rows)
	if err != nil {
		return fmt.Errorf("record progress of %s.%s: %w", schema, name, err)
	}
	return nil
}

// SetVerified records the verification result of one table.
func (s *Store) SetVerified(ctx context.Context, q pg.Querier, schema, name string, ok bool, errMsg string) error {
	_, err := q.Exec(ctx, fmt.Sprintf(
		`UPDATE %s SET verified_ok = $3, error_msg = NULLIF($4, ''), updated_at = now() WHERE schema_name = $1 AND table_name = $2`,
		s.progress()), schema, name, ok, errMsg)
	if err != nil {
		return fmt.Errorf("record verification of %s.%s: %w", schema, name, err)
	}
	return nil
}

// ListTables returns every table_progress row ordered by name.
func (s *Store) ListTables(ctx context.Context, q pg.Querier) ([]TableProgress, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT schema_name, table_name, has_pk, replica_id, snapshot_status,
    rows_copied, verified_ok, COALESCE(error_msg, ''), updated_at FROM %s ORDER BY schema_name, table_name`, s.progress()))
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", s.progress(), err)
	}
	defer rows.Close()
	var out []TableProgress
	for rows.Next() {
		var t TableProgress
		if err := rows.Scan(&t.Schema, &t.Name, &t.HasPK, &t.ReplicaID, &t.SnapshotStatus,
			&t.RowsCopied, &t.VerifiedOK, &t.ErrorMsg, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan %s: %w", s.progress(), err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func parseLSNPtr(text *string) (*pglogrepl.LSN, error) {
	if text == nil {
		return nil, nil
	}
	lsn, err := pglogrepl.ParseLSN(*text)
	if err != nil {
		return nil, fmt.Errorf("parse stored LSN %q: %w", *text, err)
	}
	return &lsn, nil
}
