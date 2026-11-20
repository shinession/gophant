package multidb

import (
	"context"
	"fmt"

	"github.com/shinession/gophant/internal/catalog"
	"github.com/shinession/gophant/internal/pg"
)

// skipReasonEmpty is why a database without a table in scope is left out of every stage.
const skipReasonEmpty = "no table in scope, nothing to copy"

// nothingToMigrate reports whether the database has no table in scope on s1. Such a database is skipped
// by the precheck and never gets a migration state, so the later stages must tell it apart from a
// database that has tables but was blocked or forgotten: the cutover must not leave that one behind.
func (f *Fleet) nothingToMigrate(ctx context.Context, d *Database) (bool, error) {
	if !d.CanConnect {
		return false, fmt.Errorf("the source role may not connect to database %s", d.Name)
	}
	conn, err := pg.Connect(ctx, d.Cfg.Source.DSN, "gophant-check")
	if err != nil {
		return false, fmt.Errorf("source: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	tables, err := catalog.ListTables(ctx, conn, &d.Cfg.Filter)
	if err != nil {
		return false, fmt.Errorf("database %q: %w", d.Name, err)
	}
	return len(tables) == 0, nil
}
