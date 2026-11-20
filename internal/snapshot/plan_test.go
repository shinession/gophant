package snapshot

import (
	"testing"

	"github.com/shinession/gophant/internal/catalog"
)

func TestCopySQLBinaryExcludesGeneratedColumns(t *testing.T) {
	tbl := catalog.Table{
		Schema:     "public",
		Name:       "orders",
		CopyFormat: catalog.FormatBinary,
		Columns: []catalog.Column{
			{Name: "order_id"},
			{Name: "amount"},
			{Name: "total_with_tax", Generated: 's'},
			{Name: "Mixed Case"},
		},
	}
	out, in := copySQL(tbl)
	wantOut := `COPY "public"."orders" ("order_id", "amount", "Mixed Case") TO STDOUT (FORMAT BINARY)`
	wantIn := `COPY "public"."orders" ("order_id", "amount", "Mixed Case") FROM STDIN (FORMAT BINARY)`
	if out != wantOut {
		t.Errorf("out = %s\nwant  %s", out, wantOut)
	}
	if in != wantIn {
		t.Errorf("in = %s\nwant %s", in, wantIn)
	}
}

func TestCopySQLTextFormat(t *testing.T) {
	tbl := catalog.Table{Schema: "s", Name: "t", CopyFormat: catalog.FormatText, Columns: []catalog.Column{{Name: "a"}}}
	out, in := copySQL(tbl)
	if out != `COPY "s"."t" ("a") TO STDOUT (FORMAT TEXT)` || in != `COPY "s"."t" ("a") FROM STDIN (FORMAT TEXT)` {
		t.Errorf("unexpected text COPY statements:\n%s\n%s", out, in)
	}
}

func TestCopySQLDefaultsToBinaryAndHandlesNoColumns(t *testing.T) {
	out, _ := copySQL(catalog.Table{Schema: "s", Name: "empty"})
	if out != `COPY "s"."empty" TO STDOUT (FORMAT BINARY)` {
		t.Errorf("a table without columns needs no column list: %s", out)
	}
}

func TestCopySQLQuotesHostileIdentifiers(t *testing.T) {
	tbl := catalog.Table{Schema: `we"ird`, Name: `t; DROP TABLE x`, Columns: []catalog.Column{{Name: `c"ol`}}}
	out, _ := copySQL(tbl)
	want := `COPY "we""ird"."t; DROP TABLE x" ("c""ol") TO STDOUT (FORMAT BINARY)`
	if out != want {
		t.Errorf("identifiers must be quoted:\n got %s\nwant %s", out, want)
	}
}

func TestSortLargestFirst(t *testing.T) {
	tables := []catalog.Table{
		{Schema: "public", Name: "small", TotalBytes: 10},
		{Schema: "public", Name: "big", TotalBytes: 1000},
		{Schema: "public", Name: "b_mid", TotalBytes: 100},
		{Schema: "public", Name: "a_mid", TotalBytes: 100},
	}
	sortLargestFirst(tables)
	var got []string
	for _, tbl := range tables {
		got = append(got, tbl.Name)
	}
	want := []string{"big", "a_mid", "b_mid", "small"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestSnapshotNamePattern(t *testing.T) {
	for _, ok := range []string{"00000003-0000001B-1", "00000A3F-00000042-1", "0000000F-0000000A"} {
		if !snapshotNamePattern.MatchString(ok) {
			t.Errorf("%q must be accepted", ok)
		}
	}
	for _, bad := range []string{"", "x'; DROP TABLE t; --", "00000003 0000001B", "00000003-0000001B-1'", "snapshot"} {
		if snapshotNamePattern.MatchString(bad) {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
