package multidb

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/pg"
)

// Found is a database of s1 that is a candidate for the migration.
type Found struct {
	Name string
	// CanConnect says whether the source role has the CONNECT privilege on it.
	CanConnect bool
}

// Discover lists the databases of s1 that are not templates and accept connections.
func Discover(ctx context.Context, q pg.Querier) ([]Found, error) {
	rows, err := q.Query(ctx, `SELECT datname, has_database_privilege(datname, 'CONNECT')
FROM pg_database WHERE NOT datistemplate AND datallowconn ORDER BY datname`)
	if err != nil {
		return nil, fmt.Errorf("list the databases of s1: %w", err)
	}
	defer rows.Close()
	var out []Found
	for rows.Next() {
		var f Found
		if err := rows.Scan(&f.Name, &f.CanConnect); err != nil {
			return nil, fmt.Errorf("list the databases of s1: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the databases of s1: %w", err)
	}
	return out, nil
}

// Selection is what databases.include, databases.exclude and --database leave of the databases found.
type Selection struct {
	Selected []Found
	// LeftOut are the names that were found and filtered out, for the report.
	LeftOut []string
}

// Select applies the databases section, then the names given with --database (none means all). A
// name given with --database has to be a database that was found and that the configuration lets
// through: asking for something that is not part of the migration is an error, not a silent no-op.
func Select(found []Found, d *config.Databases, only []string) (Selection, error) {
	names := make([]string, len(found))
	for i, f := range found {
		names[i] = f.Name
	}
	for _, name := range only {
		i := slices.Index(names, name)
		switch {
		case i < 0:
			return Selection{}, fmt.Errorf("database %q is not on s1 or does not accept connections; found: %s", name, joinNames(names))
		case !d.Matches(name):
			return Selection{}, fmt.Errorf("database %q is left out by databases.include or databases.exclude", name)
		}
	}

	var sel Selection
	for _, f := range found {
		if d.Matches(f.Name) && (len(only) == 0 || slices.Contains(only, f.Name)) {
			sel.Selected = append(sel.Selected, f)
		} else {
			sel.LeftOut = append(sel.LeftOut, f.Name)
		}
	}
	if len(sel.Selected) == 0 {
		return Selection{}, fmt.Errorf("no database is left to migrate: s1 has %s; check databases.include and databases.exclude", joinNames(names))
	}
	return sel, nil
}

func joinNames(names []string) string {
	if len(names) == 0 {
		return "no database"
	}
	return strings.Join(names, ", ")
}
