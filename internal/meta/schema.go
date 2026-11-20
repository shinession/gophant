// Package meta persists the migration state on the target database (s3), in the dedicated
// gophant_meta schema, so no external state store is needed.
package meta

import (
	"context"
	"fmt"

	"github.com/shinession/gophant/internal/pg"
)

// Stage is the stage of the five-stage migration state machine.
type Stage string

// The stages stored in sync_state.stage.
const (
	StagePrecheck  Stage = "PRECHECK"
	StageSnapshot  Stage = "SNAPSHOT"
	StageCDCStream Stage = "CDC_STREAM"
	StageCutover   Stage = "CUTOVER"
	StageVerify    Stage = "VERIFY"
	StageDone      Stage = "DONE"
)

// Valid reports whether s is one of the known stages.
func (s Stage) Valid() bool {
	switch s {
	case StagePrecheck, StageSnapshot, StageCDCStream, StageCutover, StageVerify, StageDone:
		return true
	}
	return false
}

// Snapshot status values stored in table_progress.snapshot_status.
const (
	StatusPending = "PENDING"
	StatusRunning = "RUNNING"
	StatusDone    = "DONE"
	StatusFailed  = "FAILED"
)

// DDL returns the statements that create the meta schema and its two tables. They are idempotent.
func DDL(schema string) []string {
	s := pg.Ident(schema)
	return []string{
		fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, s),

		// Single-row global state; id is always 1.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.sync_state (
    id              INT PRIMARY KEY CHECK (id = 1),
    stage           TEXT NOT NULL,
    slot_name       TEXT NOT NULL,
    publication     TEXT NOT NULL,
    consistent_lsn  PG_LSN,
    applied_lsn     PG_LSN,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
)`, s),

		// Per-table copy and verification progress.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.table_progress (
    schema_name     TEXT NOT NULL,
    table_name      TEXT NOT NULL,
    has_pk          BOOLEAN NOT NULL DEFAULT true,
    replica_id      CHAR(1) NOT NULL DEFAULT 'd',
    snapshot_status TEXT NOT NULL DEFAULT 'PENDING',
    rows_copied     BIGINT NOT NULL DEFAULT 0,
    verified_ok     BOOLEAN,
    error_msg       TEXT,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (schema_name, table_name)
)`, s),
	}
}

// Ensure creates the meta schema and tables when they do not exist yet.
func Ensure(ctx context.Context, q pg.Querier, schema string) error {
	for _, stmt := range DDL(schema) {
		if _, err := q.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("initialize %s: %w", schema, err)
		}
	}
	return nil
}
