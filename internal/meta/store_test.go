package meta

import (
	"strings"
	"testing"

	"github.com/jackc/pglogrepl"
)

func TestDDLUsesTheConfiguredSchemaAndIsIdempotent(t *testing.T) {
	stmts := DDL("my_meta")
	if len(stmts) != 3 {
		t.Fatalf("expected 3 statements, got %d", len(stmts))
	}
	all := strings.Join(stmts, "\n")
	for _, want := range []string{
		`CREATE SCHEMA IF NOT EXISTS "my_meta"`,
		`CREATE TABLE IF NOT EXISTS "my_meta".sync_state`,
		`CREATE TABLE IF NOT EXISTS "my_meta".table_progress`,
		"CHECK (id = 1)",
		"applied_lsn     PG_LSN",
		"PRIMARY KEY (schema_name, table_name)",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("DDL does not contain %q:\n%s", want, all)
		}
	}
	for _, s := range stmts {
		if !strings.Contains(s, "IF NOT EXISTS") {
			t.Errorf("statement is not idempotent: %s", s)
		}
	}
}

func TestStageValid(t *testing.T) {
	for _, s := range []Stage{StagePrecheck, StageSnapshot, StageCDCStream, StageCutover, StageVerify, StageDone} {
		if !s.Valid() {
			t.Errorf("%s should be valid", s)
		}
	}
	if Stage("BOGUS").Valid() {
		t.Error("BOGUS must not be valid")
	}
}

func TestStartLSNPrefersTheAppliedPosition(t *testing.T) {
	consistent := pglogrepl.LSN(0x100)
	applied := pglogrepl.LSN(0x200)

	st := &SyncState{ConsistentLSN: &consistent}
	if lsn, ok := st.StartLSN(); !ok || lsn != consistent {
		t.Errorf("without applied_lsn the consistent point is used, got %v %v", lsn, ok)
	}

	st.AppliedLSN = &applied
	if lsn, ok := st.StartLSN(); !ok || lsn != applied {
		t.Errorf("applied_lsn takes precedence, got %v %v", lsn, ok)
	}

	if _, ok := (&SyncState{}).StartLSN(); ok {
		t.Error("a state without any position has no start LSN")
	}
}

func TestAppliedLSNStatementIsGuarded(t *testing.T) {
	stmt := NewStore("gophant_meta").AppliedLSNStatement()
	for _, want := range []string{
		`UPDATE "gophant_meta"."sync_state"`,
		"applied_lsn IS NULL OR applied_lsn < $1::pg_lsn",
		"WHERE id = 1",
	} {
		if !strings.Contains(stmt, want) {
			t.Errorf("statement does not contain %q:\n%s", want, stmt)
		}
	}
}

func TestParseLSNPtr(t *testing.T) {
	if lsn, err := parseLSNPtr(nil); err != nil || lsn != nil {
		t.Errorf("nil text must give a nil LSN, got %v %v", lsn, err)
	}
	text := "16/B374D848"
	lsn, err := parseLSNPtr(&text)
	if err != nil || lsn == nil || lsn.String() != text {
		t.Errorf("parseLSNPtr(%q) = %v %v", text, lsn, err)
	}
	bad := "not-an-lsn"
	if _, err := parseLSNPtr(&bad); err == nil {
		t.Error("an invalid LSN must be rejected")
	}
}
