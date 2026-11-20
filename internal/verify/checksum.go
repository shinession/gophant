package verify

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/pg"
)

// pkColumn is one column of a primary key together with the type used to cast key values that are
// passed back as parameters.
type pkColumn struct {
	Name string
	Type string
}

const primaryKeySQL = `
SELECT a.attname, format_type(a.atttypid, a.atttypmod)
FROM pg_index i
JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
WHERE i.indrelid = to_regclass($1) AND i.indisprimary
ORDER BY array_position(i.indkey::int2[], a.attnum)`

// primaryKey returns the primary key columns of a table in key order; empty without a primary key.
func primaryKey(ctx context.Context, q pg.Querier, table string) ([]pkColumn, error) {
	rows, err := q.Query(ctx, primaryKeySQL, table)
	if err != nil {
		return nil, fmt.Errorf("read the primary key of %s: %w", table, err)
	}
	defer rows.Close()
	var cols []pkColumn
	for rows.Next() {
		var c pkColumn
		if err := rows.Scan(&c.Name, &c.Type); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

// A key tuple is carried as the text of its columns; it is cast back with the type of the column.
type keyTuple []string

func (k keyTuple) String() string { return "(" + strings.Join(k, ", ") + ")" }

// quotedList returns "a", "b" for the primary key columns.
func quotedList(pk []pkColumn) string {
	names := make([]string, len(pk))
	for i, c := range pk {
		names[i] = pg.Ident(c.Name)
	}
	return strings.Join(names, ", ")
}

// rowValue renders ROW($n::type, ...) for a key tuple, with parameters numbered from first.
func rowValue(pk []pkColumn, first int) string {
	parts := make([]string, len(pk))
	for i, c := range pk {
		parts[i] = "$" + strconv.Itoa(first+i) + "::" + c.Type
	}
	return "ROW(" + strings.Join(parts, ", ") + ")"
}

func args(k keyTuple) []any {
	out := make([]any, len(k))
	for i, v := range k {
		out[i] = v
	}
	return out
}

// boundarySQL selects the primary key of the row that ends the next chunk: chunkSize rows after the
// cursor in key order. With a nil cursor it starts at the beginning of the table.
func boundarySQL(table string, pk []pkColumn, cursor keyTuple, chunkSize int) (string, []any) {
	casts := make([]string, len(pk))
	for i, c := range pk {
		casts[i] = pg.Ident(c.Name) + "::text"
	}
	sql := "SELECT " + strings.Join(casts, ", ") + " FROM ONLY " + table
	var a []any
	if cursor != nil {
		sql += " WHERE ROW(" + quotedList(pk) + ") > " + rowValue(pk, 1)
		a = args(cursor)
	}
	sql += " ORDER BY " + quotedList(pk) + " OFFSET " + strconv.Itoa(chunkSize-1) + " LIMIT 1"
	return sql, a
}

// boundaries splits the table into chunks of chunkSize rows by primary key and returns the upper
// bound (inclusive) of every chunk except the last, which is open-ended.
func boundaries(ctx context.Context, q pg.Querier, table string, pk []pkColumn, chunkSize int) ([]keyTuple, error) {
	var bounds []keyTuple
	var cursor keyTuple
	for {
		sql, a := boundarySQL(table, pk, cursor, chunkSize)
		vals := make([]*string, len(pk))
		scan := make([]any, len(pk))
		for i := range vals {
			scan[i] = &vals[i]
		}
		err := q.QueryRow(ctx, sql, a...).Scan(scan...)
		if errors.Is(err, pgx.ErrNoRows) {
			return bounds, nil
		}
		if err != nil {
			return nil, fmt.Errorf("find the chunk boundaries of %s: %w", table, err)
		}
		next := make(keyTuple, len(pk))
		for i, v := range vals {
			next[i] = *v
		}
		bounds = append(bounds, next)
		cursor = next
	}
}

// chunk is one primary key range (lo, hi]; a nil bound is open.
type chunk struct {
	Lo, Hi keyTuple
}

func (c chunk) String() string {
	switch {
	case c.Lo == nil && c.Hi == nil:
		return "the whole table"
	case c.Lo == nil:
		return "key <= " + c.Hi.String()
	case c.Hi == nil:
		return "key > " + c.Lo.String()
	}
	return "key > " + c.Lo.String() + " and <= " + c.Hi.String()
}

// chunksFromBounds turns the boundaries into ranges.
func chunksFromBounds(bounds []keyTuple) []chunk {
	chunks := make([]chunk, 0, len(bounds)+1)
	var lo keyTuple
	for _, b := range bounds {
		chunks = append(chunks, chunk{Lo: lo, Hi: b})
		lo = b
	}
	return append(chunks, chunk{Lo: lo})
}

// hashSQL builds the query that fingerprints a chunk. Every row is rendered with its whole-row text
// form and reduced to 64 bits of its MD5; the fingerprint is the row count and the sum of those
// hashes. A sum is independent of the row order and, unlike an XOR, does not cancel two identical
// rows. Both servers run the same text, so equal data gives equal fingerprints.
func hashSQL(table string, pk []pkColumn, c chunk) (string, []any) {
	sql := `SELECT count(*), COALESCE(sum((('x' || substr(md5(r::text), 1, 16))::bit(64)::bigint)::numeric), 0)::text FROM ONLY ` + table + ` AS r`
	var conds []string
	var a []any
	if c.Lo != nil {
		conds = append(conds, "ROW("+quotedList(pk)+") > "+rowValue(pk, 1))
		a = append(a, args(c.Lo)...)
	}
	if c.Hi != nil {
		conds = append(conds, "ROW("+quotedList(pk)+") <= "+rowValue(pk, len(a)+1))
		a = append(a, args(c.Hi)...)
	}
	if len(conds) > 0 {
		sql += " WHERE " + strings.Join(conds, " AND ")
	}
	return sql, a
}

// fingerprint is the result of hashSQL.
type fingerprint struct {
	Rows int64
	Sum  string
}

func fingerprintOf(ctx context.Context, q pg.Querier, table string, pk []pkColumn, c chunk) (fingerprint, error) {
	sql, a := hashSQL(table, pk, c)
	var f fingerprint
	if err := q.QueryRow(ctx, sql, a...).Scan(&f.Rows, &f.Sum); err != nil {
		return f, err
	}
	return f, nil
}

// pickChunks returns the indexes of the chunks to hash: all of them, or a random sample of
// ceil(ratio * n) chunks, at least one.
func pickChunks(n int, mode string, ratio float64) []int {
	all := make([]int, n)
	for i := range all {
		all[i] = i
	}
	if mode == config.VerifyModeFullHash || n == 0 {
		return all
	}
	k := min(n, max(1, int(float64(n)*ratio+0.999999)))
	if k == n {
		return all
	}
	rand.Shuffle(n, func(i, j int) { all[i], all[j] = all[j], all[i] })
	picked := all[:k]
	slices.Sort(picked)
	return picked
}

// sessionSettings make the text form of every value independent of the settings of the two servers.
var sessionSettings = []string{
	"SET TimeZone = 'UTC'",
	"SET DateStyle = 'ISO, MDY'",
	"SET IntervalStyle = 'postgres'",
	"SET extra_float_digits = 1",
	"SET bytea_output = 'hex'",
	"SET lc_monetary = 'C'",
}

func setSession(ctx context.Context, conn *pgx.Conn) error {
	for _, s := range sessionSettings {
		if _, err := conn.Exec(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}
