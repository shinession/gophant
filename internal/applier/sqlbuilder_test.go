package applier

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pglogrepl"
	"github.com/shinession/gophant/internal/cdc"
)

func txt(s string) cdc.Value { return cdc.Value{Kind: cdc.ValueText, Text: s} }
func null() cdc.Value        { return cdc.Value{Kind: cdc.ValueNull} }
func toast() cdc.Value       { return cdc.Value{Kind: cdc.ValueToast} }

func plainMeta(cols ...string) *TableMeta {
	tm := &TableMeta{Columns: map[string]ColumnMeta{}, HasPK: true}
	for _, c := range cols {
		tm.Columns[c] = ColumnMeta{}
	}
	return tm
}

// ordinary table: id (key), name, note
func ordinaryRel() *cdc.Relation {
	return &cdc.Relation{
		ID: 1, Schema: "public", Name: "customers", ReplicaIdentity: 'd',
		Columns: []cdc.RelColumn{{Name: "id", Key: true}, {Name: "name"}, {Name: "note"}},
	}
}

func TestBuildInsertOrdinaryTable(t *testing.T) {
	rel := ordinaryRel()
	st, err := BuildInsert(rel, plainMeta("id", "name", "note"), cdc.Tuple{txt("7"), txt("Ann"), null()})
	if err != nil {
		t.Fatal(err)
	}
	if want := `INSERT INTO "public"."customers" ("id", "name", "note") VALUES ($1, $2, $3)`; st.SQL != want {
		t.Errorf("SQL = %s\nwant  %s", st.SQL, want)
	}
	if want := []any{"7", "Ann", nil}; !reflect.DeepEqual(st.Args, want) {
		t.Errorf("args = %#v, want %#v", st.Args, want)
	}
	if st.Kind != KindInsert {
		t.Errorf("kind = %v", st.Kind)
	}
}

func TestBuildInsertSkipsGeneratedColumns(t *testing.T) {
	rel := &cdc.Relation{Schema: "public", Name: "orders", ReplicaIdentity: 'd',
		Columns: []cdc.RelColumn{{Name: "order_id", Key: true}, {Name: "amount"}, {Name: "total_with_tax"}}}
	tm := plainMeta("order_id", "amount")
	tm.Columns["total_with_tax"] = ColumnMeta{Generated: true}

	st, err := BuildInsert(rel, tm, cdc.Tuple{txt("1"), txt("10.00"), txt("12.000")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(st.SQL, "total_with_tax") {
		t.Errorf("a generated column must not be written: %s", st.SQL)
	}
	if len(st.Args) != 2 {
		t.Errorf("args = %#v", st.Args)
	}
}

func TestBuildInsertIdentityAlwaysNeedsOverriding(t *testing.T) {
	rel := &cdc.Relation{Schema: "public", Name: "t", ReplicaIdentity: 'd',
		Columns: []cdc.RelColumn{{Name: "id", Key: true}, {Name: "v"}}}
	tm := plainMeta("id", "v")
	tm.Columns["id"] = ColumnMeta{IdentityAlways: true}

	st, err := BuildInsert(rel, tm, cdc.Tuple{txt("5"), txt("x")})
	if err != nil {
		t.Fatal(err)
	}
	if want := `INSERT INTO "public"."t" ("id", "v") OVERRIDING SYSTEM VALUE VALUES ($1, $2)`; st.SQL != want {
		t.Errorf("SQL = %s\nwant  %s", st.SQL, want)
	}

	plain, err := BuildInsert(ordinaryRel(), plainMeta("id", "name", "note"), cdc.Tuple{txt("1"), txt("a"), txt("b")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.SQL, "OVERRIDING") {
		t.Errorf("OVERRIDING must only appear for identity-always columns: %s", plain.SQL)
	}
}

func TestBuildInsertErrors(t *testing.T) {
	rel := ordinaryRel()
	if _, err := BuildInsert(rel, plainMeta("id", "name", "note"), cdc.Tuple{txt("1"), toast(), txt("x")}); err == nil {
		t.Error("an unchanged TOAST value in an insert must be an error")
	}
	if _, err := BuildInsert(rel, plainMeta("id", "name"), cdc.Tuple{txt("1"), txt("a"), txt("b")}); err == nil || !strings.Contains(err.Error(), "schema drift") {
		t.Errorf("a column missing on s3 must be reported as schema drift, got %v", err)
	}
	if _, err := BuildInsert(rel, plainMeta("id", "name", "note"), cdc.Tuple{txt("1")}); err == nil {
		t.Error("a short tuple must be an error")
	}
}

func TestBuildInsertTableWithoutWritableColumns(t *testing.T) {
	rel := &cdc.Relation{Schema: "s", Name: "t", Columns: nil}
	st, err := BuildInsert(rel, plainMeta(), cdc.Tuple{})
	if err != nil {
		t.Fatal(err)
	}
	if st.SQL != `INSERT INTO "s"."t" DEFAULT VALUES` {
		t.Errorf("SQL = %s", st.SQL)
	}
}

// Hard rule: a column that arrives as an unchanged TOAST value must never be part of SET.
func TestBuildUpdateSkipsUnchangedToastColumns(t *testing.T) {
	rel := ordinaryRel()
	st, skip, err := BuildUpdate(rel, plainMeta("id", "name", "note"), cdc.OldNone, nil,
		cdc.Tuple{txt("7"), txt("Bob"), toast()})
	if err != nil || skip {
		t.Fatalf("err = %v, skip = %v", err, skip)
	}
	if strings.Contains(st.SQL, `"note"`) {
		t.Errorf("the unchanged TOAST column must not be assigned: %s", st.SQL)
	}
	want := `UPDATE ONLY "public"."customers" SET "id" = $1, "name" = $2 WHERE "id" = $3`
	if st.SQL != want {
		t.Errorf("SQL = %s\nwant  %s", st.SQL, want)
	}
	if !reflect.DeepEqual(st.Args, []any{"7", "Bob", "7"}) {
		t.Errorf("args = %#v", st.Args)
	}
}

func TestBuildUpdateOnlyToastColumnsLeftIsSkipped(t *testing.T) {
	rel := &cdc.Relation{Schema: "s", Name: "t", ReplicaIdentity: 'd',
		Columns: []cdc.RelColumn{{Name: "id", Key: true}, {Name: "doc"}}}
	tm := plainMeta("id", "doc")
	// The key column is identity-always here, so nothing remains to assign besides the TOAST value.
	tm.Columns["id"] = ColumnMeta{IdentityAlways: true}
	_, skip, err := BuildUpdate(rel, tm, cdc.OldNone, nil, cdc.Tuple{txt("1"), toast()})
	if err != nil || !skip {
		t.Errorf("an update without anything to assign must be skipped, skip = %v err = %v", skip, err)
	}
}

func TestBuildUpdateUsesOldKeyWhenTheKeyChanged(t *testing.T) {
	rel := ordinaryRel()
	oldKey := cdc.Tuple{txt("7"), null(), null()} // key-only old tuple: non-key columns are not meaningful
	st, _, err := BuildUpdate(rel, plainMeta("id", "name", "note"), cdc.OldKey, oldKey,
		cdc.Tuple{txt("8"), txt("Bob"), txt("n")})
	if err != nil {
		t.Fatal(err)
	}
	want := `UPDATE ONLY "public"."customers" SET "id" = $1, "name" = $2, "note" = $3 WHERE "id" = $4`
	if st.SQL != want {
		t.Errorf("SQL = %s\nwant  %s", st.SQL, want)
	}
	if !reflect.DeepEqual(st.Args, []any{"8", "Bob", "n", "7"}) {
		t.Errorf("the row must be located by the OLD key: %#v", st.Args)
	}
}

func TestBuildUpdateCompositePrimaryKey(t *testing.T) {
	rel := &cdc.Relation{Schema: "public", Name: "order_items", ReplicaIdentity: 'd',
		Columns: []cdc.RelColumn{{Name: "order_id", Key: true}, {Name: "line_no", Key: true}, {Name: "qty"}}}
	st, _, err := BuildUpdate(rel, plainMeta("order_id", "line_no", "qty"), cdc.OldNone, nil,
		cdc.Tuple{txt("5"), txt("2"), txt("9")})
	if err != nil {
		t.Fatal(err)
	}
	if want := `WHERE "order_id" = $4 AND "line_no" = $5`; !strings.HasSuffix(st.SQL, want) {
		t.Errorf("both key columns must locate the row: %s", st.SQL)
	}
	if !reflect.DeepEqual(st.Args, []any{"5", "2", "9", "5", "2"}) {
		t.Errorf("args = %#v", st.Args)
	}

	del, err := BuildDelete(rel, plainMeta("order_id", "line_no", "qty"), cdc.Tuple{txt("5"), txt("2"), null()})
	if err != nil {
		t.Fatal(err)
	}
	if want := `DELETE FROM ONLY "public"."order_items" WHERE "order_id" = $1 AND "line_no" = $2`; del.SQL != want {
		t.Errorf("SQL = %s\nwant  %s", del.SQL, want)
	}
}

// Without a primary key the row is located by its full old content: NULL needs IS NULL, never "= NULL",
// and the ctid guard makes sure that exactly one of several identical rows is changed.
func TestBuildUpdateAndDeleteWithoutPrimaryKeyHandleNullAndDuplicates(t *testing.T) {
	rel := &cdc.Relation{Schema: "public", Name: "events", ReplicaIdentity: 'f',
		Columns: []cdc.RelColumn{{Name: "kind", Key: true}, {Name: "n", Key: true}, {Name: "note", Key: true}}}
	tm := plainMeta("kind", "n", "note")
	tm.HasPK = false
	old := cdc.Tuple{txt("login"), null(), txt("x")}

	up, _, err := BuildUpdate(rel, tm, cdc.OldFull, old, cdc.Tuple{txt("login"), txt("3"), txt("x")})
	if err != nil {
		t.Fatal(err)
	}
	wantUp := `UPDATE ONLY "public"."events" SET "kind" = $1, "n" = $2, "note" = $3 ` +
		`WHERE ctid = (SELECT ctid FROM ONLY "public"."events" WHERE "kind" = $4 AND "n" IS NULL AND "note" = $5 LIMIT 1)`
	if up.SQL != wantUp {
		t.Errorf("SQL = %s\nwant  %s", up.SQL, wantUp)
	}
	if !reflect.DeepEqual(up.Args, []any{"login", "3", "x", "login", "x"}) {
		t.Errorf("a NULL in the identity must not become an argument: %#v", up.Args)
	}
	if strings.Contains(up.SQL, "= NULL") {
		t.Errorf("= NULL never matches: %s", up.SQL)
	}

	del, err := BuildDelete(rel, tm, old)
	if err != nil {
		t.Fatal(err)
	}
	wantDel := `DELETE FROM ONLY "public"."events" ` +
		`WHERE ctid = (SELECT ctid FROM ONLY "public"."events" WHERE "kind" = $1 AND "n" IS NULL AND "note" = $2 LIMIT 1)`
	if del.SQL != wantDel {
		t.Errorf("SQL = %s\nwant  %s", del.SQL, wantDel)
	}
}

func TestFullIdentityWithPrimaryKeyNeedsNoCtidGuard(t *testing.T) {
	rel := &cdc.Relation{Schema: "public", Name: "t", ReplicaIdentity: 'f',
		Columns: []cdc.RelColumn{{Name: "id", Key: true}, {Name: "v", Key: true}}}
	del, err := BuildDelete(rel, plainMeta("id", "v"), cdc.Tuple{txt("1"), txt("a")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(del.SQL, "ctid") {
		t.Errorf("a table with a primary key identifies rows without ctid: %s", del.SQL)
	}
}

func TestPartitionedRootUsesTableoidAndNoOnly(t *testing.T) {
	rel := &cdc.Relation{Schema: "public", Name: "metrics", ReplicaIdentity: 'f',
		Columns: []cdc.RelColumn{{Name: "bucket", Key: true}, {Name: "val", Key: true}}}
	tm := plainMeta("bucket", "val")
	tm.HasPK = false
	tm.Partitioned = true
	del, err := BuildDelete(rel, tm, cdc.Tuple{txt("2026-01-01"), txt("1")})
	if err != nil {
		t.Fatal(err)
	}
	want := `DELETE FROM "public"."metrics" WHERE (tableoid, ctid) = ` +
		`(SELECT tableoid, ctid FROM "public"."metrics" WHERE "bucket" = $1 AND "val" = $2 LIMIT 1)`
	if del.SQL != want {
		t.Errorf("SQL = %s\nwant  %s", del.SQL, want)
	}

	pk := plainMeta("bucket", "val")
	pk.Partitioned = true
	relPK := &cdc.Relation{Schema: "public", Name: "metrics", ReplicaIdentity: 'd',
		Columns: []cdc.RelColumn{{Name: "bucket", Key: true}, {Name: "val"}}}
	up, _, err := BuildUpdate(relPK, pk, cdc.OldNone, nil, cdc.Tuple{txt("2026-01-01"), txt("2")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(up.SQL, `UPDATE "public"."metrics" SET`) {
		t.Errorf("a partitioned root must not use ONLY (it has no rows of its own): %s", up.SQL)
	}
}

func TestTextCompareColumnsInTheIdentity(t *testing.T) {
	rel := &cdc.Relation{Schema: "public", Name: "log", ReplicaIdentity: 'f',
		Columns: []cdc.RelColumn{{Name: "kind", Key: true}, {Name: "payload", Key: true}}}
	tm := plainMeta("kind")
	tm.HasPK = false
	tm.Columns["payload"] = ColumnMeta{TextCompare: true}
	del, err := BuildDelete(rel, tm, cdc.Tuple{txt("a"), txt(`{"x": 1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(del.SQL, `"payload"::text = $2`) {
		t.Errorf("json has no equality operator, the text form must be compared: %s", del.SQL)
	}
}

func TestBuildUpdateIdentityAlwaysColumns(t *testing.T) {
	rel := &cdc.Relation{Schema: "public", Name: "t", ReplicaIdentity: 'd',
		Columns: []cdc.RelColumn{{Name: "id", Key: true}, {Name: "v"}}}
	tm := plainMeta("id", "v")
	tm.Columns["id"] = ColumnMeta{IdentityAlways: true}

	// Unchanged key: the identity column is simply not assigned.
	st, _, err := BuildUpdate(rel, tm, cdc.OldNone, nil, cdc.Tuple{txt("5"), txt("x")})
	if err != nil {
		t.Fatal(err)
	}
	if want := `UPDATE ONLY "public"."t" SET "v" = $1 WHERE "id" = $2`; st.SQL != want {
		t.Errorf("SQL = %s\nwant  %s", st.SQL, want)
	}

	// A changed identity value cannot be replayed and must fail loudly instead of diverging silently.
	_, _, err = BuildUpdate(rel, tm, cdc.OldKey, cdc.Tuple{txt("5"), null()}, cdc.Tuple{txt("6"), txt("x")})
	if err == nil || !strings.Contains(err.Error(), "identity column") {
		t.Errorf("expected an identity column error, got %v", err)
	}
}

func TestBuildUpdateAndDeleteRefuseTablesWithoutIdentity(t *testing.T) {
	rel := &cdc.Relation{Schema: "public", Name: "t", ReplicaIdentity: 'n', Columns: []cdc.RelColumn{{Name: "a"}}}
	tm := plainMeta("a")
	if _, _, err := BuildUpdate(rel, tm, cdc.OldNone, nil, cdc.Tuple{txt("1")}); err == nil {
		t.Error("an update without replica identity columns must be an error")
	}
	if _, err := BuildDelete(rel, tm, cdc.Tuple{txt("1")}); err == nil {
		t.Error("a delete without replica identity columns must be an error")
	}
	if _, err := BuildDelete(ordinaryRel(), plainMeta("id", "name", "note"), nil); err == nil {
		t.Error("a delete without old tuple must be an error")
	}
}

func TestIdentityColumnAsUnchangedToastIsAnError(t *testing.T) {
	rel := ordinaryRel()
	_, _, err := BuildUpdate(rel, plainMeta("id", "name", "note"), cdc.OldNone, nil, cdc.Tuple{toast(), txt("a"), txt("b")})
	if err == nil {
		t.Error("a key that arrives as an unchanged TOAST value cannot locate the row")
	}
}

func TestBuildTruncate(t *testing.T) {
	a := &cdc.Relation{Schema: "public", Name: "a"}
	b := &cdc.Relation{Schema: "sales", Name: `We"ird`}

	st := BuildTruncate([]*cdc.Relation{a, b}, 0)
	if want := `TRUNCATE "public"."a", "sales"."We""ird"`; st.SQL != want {
		t.Errorf("SQL = %s\nwant  %s", st.SQL, want)
	}
	st = BuildTruncate([]*cdc.Relation{a}, pglogrepl.TruncateOptionRestartIdentity|pglogrepl.TruncateOptionCascade)
	if want := `TRUNCATE "public"."a" RESTART IDENTITY CASCADE`; st.SQL != want {
		t.Errorf("SQL = %s\nwant  %s", st.SQL, want)
	}
	if st.Kind != KindTruncate {
		t.Errorf("kind = %v", st.Kind)
	}
}

func TestHostileNamesAreQuoted(t *testing.T) {
	rel := &cdc.Relation{Schema: `s"1`, Name: `t; DROP TABLE x`, ReplicaIdentity: 'd',
		Columns: []cdc.RelColumn{{Name: `c"1`, Key: true}}}
	tm := plainMeta(`c"1`)
	st, err := BuildInsert(rel, tm, cdc.Tuple{txt("1")})
	if err != nil {
		t.Fatal(err)
	}
	if want := `INSERT INTO "s""1"."t; DROP TABLE x" ("c""1") VALUES ($1)`; st.SQL != want {
		t.Errorf("SQL = %s\nwant  %s", st.SQL, want)
	}
}
