package snapshot

import (
	"context"
	"fmt"
	"sort"

	"github.com/shinession/gophant/internal/catalog"
	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/pg"
)

// Plan is the set of tables of the migration scope, read from s1.
type Plan struct {
	// All are the tables in scope, partitioned parents included.
	All []catalog.Table
	// Units are the tables that hold rows and are therefore copied: ordinary tables and the leaf
	// partitions. A partitioned parent has no rows of its own, its partitions are copied one by one,
	// which also gives parallelism inside one partitioned table.
	Units []catalog.Table
	// TextFallbacks are the tables that are copied in text instead of binary format.
	TextFallbacks []catalog.FormatFallback
}

// BuildPlan reads the tables, columns and column types of the scope from s1.
func BuildPlan(ctx context.Context, src pg.Querier, filter *config.Filter) (*Plan, error) {
	tables, err := catalog.ListTables(ctx, src, filter)
	if err != nil {
		return nil, err
	}
	if err := catalog.LoadColumns(ctx, src, tables); err != nil {
		return nil, err
	}
	types, err := catalog.LoadUserTypes(ctx, src)
	if err != nil {
		return nil, err
	}
	p := &Plan{All: tables, TextFallbacks: catalog.AssignCopyFormats(tables, types)}
	for _, t := range tables {
		if t.IsPhysical() {
			p.Units = append(p.Units, t)
		}
	}
	sortLargestFirst(p.Units)
	return p, nil
}

// sortLargestFirst orders tables so that the biggest start first; the copy then ends with the small
// tables instead of waiting for one huge table that started last.
func sortLargestFirst(tables []catalog.Table) {
	sort.SliceStable(tables, func(i, j int) bool {
		if tables[i].TotalBytes != tables[j].TotalBytes {
			return tables[i].TotalBytes > tables[j].TotalBytes
		}
		return tables[i].FullName() < tables[j].FullName()
	})
}

// copySQL returns the COPY statements that read a table from s1 and load it into s3. Generated
// columns are left out of the column list: the target computes them itself.
func copySQL(t catalog.Table) (out, in string) {
	format := string(t.CopyFormat)
	if format == "" {
		format = string(catalog.FormatBinary)
	}
	cols := columnList(t)
	out = fmt.Sprintf("COPY %s%s TO STDOUT (FORMAT %s)", t.Quoted(), cols, format)
	in = fmt.Sprintf("COPY %s%s FROM STDIN (FORMAT %s)", t.Quoted(), cols, format)
	return out, in
}

func columnList(t catalog.Table) string {
	names := t.ColumnNames()
	if len(names) == 0 {
		return "" // a table without columns still has rows to copy
	}
	quoted := make([]byte, 0, 64)
	quoted = append(quoted, " ("...)
	for i, n := range names {
		if i > 0 {
			quoted = append(quoted, ", "...)
		}
		quoted = append(quoted, pg.Ident(n)...)
	}
	quoted = append(quoted, ')')
	return string(quoted)
}
