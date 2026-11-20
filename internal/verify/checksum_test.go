package verify

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shinession/gophant/internal/config"
)

var (
	singlePK    = []pkColumn{{Name: "id", Type: "bigint"}}
	compositePK = []pkColumn{{Name: "order_id", Type: "integer"}, {Name: "line_no", Type: "integer"}}
)

func TestBoundarySQL(t *testing.T) {
	sql, a := boundarySQL(`"public"."t"`, singlePK, nil, 50000)
	want := `SELECT "id"::text FROM ONLY "public"."t" ORDER BY "id" OFFSET 49999 LIMIT 1`
	if sql != want || len(a) != 0 {
		t.Errorf("first boundary:\n got %s %v\nwant %s", sql, a, want)
	}

	sql, a = boundarySQL(`"public"."order_items"`, compositePK, keyTuple{"5", "2"}, 100)
	want = `SELECT "order_id"::text, "line_no"::text FROM ONLY "public"."order_items" ` +
		`WHERE ROW("order_id", "line_no") > ROW($1::integer, $2::integer) ORDER BY "order_id", "line_no" OFFSET 99 LIMIT 1`
	if sql != want || !reflect.DeepEqual(a, []any{"5", "2"}) {
		t.Errorf("next boundary:\n got %s %v\nwant %s", sql, a, want)
	}
}

func TestChunksFromBounds(t *testing.T) {
	chunks := chunksFromBounds([]keyTuple{{"100"}, {"200"}})
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(chunks))
	}
	if chunks[0].Lo != nil || chunks[0].Hi[0] != "100" {
		t.Errorf("first chunk: %+v", chunks[0])
	}
	if chunks[1].Lo[0] != "100" || chunks[1].Hi[0] != "200" {
		t.Errorf("middle chunk: %+v", chunks[1])
	}
	if chunks[2].Lo[0] != "200" || chunks[2].Hi != nil {
		t.Errorf("the last chunk is open-ended: %+v", chunks[2])
	}
	// A table that fits in one chunk is a single open range.
	if one := chunksFromBounds(nil); len(one) != 1 || one[0].Lo != nil || one[0].Hi != nil {
		t.Errorf("single chunk: %+v", one)
	}
}

func TestHashSQLRanges(t *testing.T) {
	prefix := `SELECT count(*), COALESCE(sum((('x' || substr(md5(r::text), 1, 16))::bit(64)::bigint)::numeric), 0)::text FROM ONLY "public"."t" AS r`

	sql, a := hashSQL(`"public"."t"`, singlePK, chunk{})
	if sql != prefix || len(a) != 0 {
		t.Errorf("the whole table has no condition: %s %v", sql, a)
	}

	sql, a = hashSQL(`"public"."t"`, singlePK, chunk{Hi: keyTuple{"100"}})
	if want := prefix + ` WHERE ROW("id") <= ROW($1::bigint)`; sql != want || !reflect.DeepEqual(a, []any{"100"}) {
		t.Errorf("first chunk:\n got %s %v\nwant %s", sql, a, want)
	}

	sql, a = hashSQL(`"public"."t"`, singlePK, chunk{Lo: keyTuple{"100"}})
	if want := prefix + ` WHERE ROW("id") > ROW($1::bigint)`; sql != want || !reflect.DeepEqual(a, []any{"100"}) {
		t.Errorf("last chunk:\n got %s %v\nwant %s", sql, a, want)
	}

	sql, a = hashSQL(`"public"."t"`, compositePK, chunk{Lo: keyTuple{"1", "2"}, Hi: keyTuple{"3", "4"}})
	want := prefix + ` WHERE ROW("order_id", "line_no") > ROW($1::integer, $2::integer) AND ROW("order_id", "line_no") <= ROW($3::integer, $4::integer)`
	if sql != want || !reflect.DeepEqual(a, []any{"1", "2", "3", "4"}) {
		t.Errorf("middle chunk with a composite key:\n got %s %v\nwant %s", sql, a, want)
	}
}

func TestPickChunks(t *testing.T) {
	if got := pickChunks(5, config.VerifyModeFullHash, 0.1); !reflect.DeepEqual(got, []int{0, 1, 2, 3, 4}) {
		t.Errorf("full hash takes every chunk, got %v", got)
	}
	if got := pickChunks(0, config.VerifyModeCountAndSample, 0.1); len(got) != 0 {
		t.Errorf("no chunks, got %v", got)
	}
	// A sample takes ceil(ratio * n) chunks, at least one, without repeats, in order.
	for _, c := range []struct {
		n     int
		ratio float64
		want  int
	}{{100, 0.1, 10}, {5, 0.1, 1}, {1, 0.1, 1}, {10, 0.25, 3}, {10, 1.0, 10}} {
		got := pickChunks(c.n, config.VerifyModeCountAndSample, c.ratio)
		if len(got) != c.want {
			t.Errorf("pickChunks(%d, %.2f) took %d chunks, want %d", c.n, c.ratio, len(got), c.want)
		}
		seen := map[int]bool{}
		prev := -1
		for _, i := range got {
			if i < 0 || i >= c.n || seen[i] || i <= prev {
				t.Errorf("pickChunks(%d, %.2f) = %v is not a sorted set of valid indexes", c.n, c.ratio, got)
				break
			}
			seen[i], prev = true, i
		}
	}
}

func TestChunkString(t *testing.T) {
	cases := map[string]chunk{
		"the whole table":            {},
		"key <= (5)":                 {Hi: keyTuple{"5"}},
		"key > (5)":                  {Lo: keyTuple{"5"}},
		"key > (1, 2) and <= (3, 4)": {Lo: keyTuple{"1", "2"}, Hi: keyTuple{"3", "4"}},
	}
	for want, c := range cases {
		if got := c.String(); got != want {
			t.Errorf("chunk.String() = %q, want %q", got, want)
		}
	}
}

func TestReportPrintAndFailed(t *testing.T) {
	r := &Report{
		Mode: config.VerifyModeFullHash,
		Results: []TableResult{
			{Name: "public.a", SourceRows: 1000, TargetRows: 1000, Chunks: 4, Hashed: 4},
			{Name: "public.b", SourceRows: 10, TargetRows: 9, Problems: []string{"row count differs: s1 has 10, s3 has 9"}},
			{Name: "public.c", SourceRows: 5, TargetRows: 5, Chunks: 1, Note: "no primary key, counted only"},
		},
		Elapsed: 1500 * time.Millisecond,
	}
	if !r.Failed() {
		t.Error("a report with a differing table has failed")
	}
	var buf bytes.Buffer
	r.Print(&buf)
	out := buf.String()
	for _, want := range []string{
		"[OK  ] public.a", "4 of 4 chunk(s) hashed", "[DIFF] public.b", "s1=10 s3=9",
		"row count differs", "no primary key, counted only", "1 with differences", "FAILED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report misses %q:\n%s", want, out)
		}
	}

	ok := &Report{Results: []TableResult{{Name: "public.a", SourceRows: 1, TargetRows: 1}}}
	if ok.Failed() {
		t.Error("a matching report has not failed")
	}
	buf.Reset()
	ok.Print(&buf)
	if !strings.Contains(buf.String(), "Result: OK") {
		t.Errorf("an OK report must say so:\n%s", buf.String())
	}
}
