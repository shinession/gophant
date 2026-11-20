package multidb

import (
	"context"
	"fmt"

	"github.com/shinession/gophant/internal/pg"
)

// dbSettings are the properties of a database that must be the same on s3: the encoding and the
// collation decide how text sorts, and a unique index depends on it.
type dbSettings struct {
	Encoding string
	Collate  string
	Ctype    string
	// Provider is "c" for the C library or "i" for ICU.
	Provider  string
	ICULocale string
	Owner     string
}

// sourceSettings reads the settings of a database from s1. The columns of pg_database differ between
// versions (datlocprovider appeared in 15, daticulocale became datlocale in 17), so the row is read as
// JSON and the keys that exist are used.
func (f *Fleet) sourceSettings(ctx context.Context, name string) (*dbSettings, error) {
	var row map[string]any
	err := f.src.QueryRow(ctx, `SELECT to_jsonb(d) || jsonb_build_object(
    'gophant_encoding', pg_encoding_to_char(d.encoding), 'gophant_owner', pg_get_userbyid(d.datdba))
FROM pg_database d WHERE d.datname = $1`, name).Scan(&row)
	if err != nil {
		return nil, fmt.Errorf("read the settings of database %q on s1: %w", name, err)
	}
	text := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := row[k].(string); ok {
				return s
			}
		}
		return ""
	}
	s := &dbSettings{
		Encoding:  text("gophant_encoding"),
		Collate:   text("datcollate"),
		Ctype:     text("datctype"),
		Provider:  text("datlocprovider"),
		ICULocale: text("daticulocale", "datlocale"),
		Owner:     text("gophant_owner"),
	}
	if s.Provider == "" {
		s.Provider = "c" // before PostgreSQL 15 there was only the C library
	}
	switch s.Provider {
	case "c", "i":
	default:
		return nil, fmt.Errorf("database %q uses the locale provider %q, which gophant cannot create on s3: create the database yourself and set databases.create_target to false", name, s.Provider)
	}
	return s, nil
}

// createDatabaseStatement returns the CREATE DATABASE statement for a database with the settings of s1.
// The text is made by the server with format(), which quotes identifiers and literals correctly, so
// no name from s1 is ever pasted into SQL here. withOwner is false when the owner role is missing on s3.
func (f *Fleet) createDatabaseStatement(ctx context.Context, name string, s *dbSettings, withOwner bool) (string, error) {
	pattern := "CREATE DATABASE %I TEMPLATE template0 ENCODING %L LC_COLLATE %L LC_CTYPE %L"
	args := []string{name, s.Encoding, s.Collate, s.Ctype}
	if s.Provider == "i" {
		pattern += " LOCALE_PROVIDER icu ICU_LOCALE %L"
		args = append(args, s.ICULocale)
	}
	if withOwner {
		pattern += " OWNER %I"
		args = append(args, s.Owner)
	}
	var stmt string
	if err := f.dst.QueryRow(ctx, `SELECT format($1::text, VARIADIC $2::text[])`, pattern, args).Scan(&stmt); err != nil {
		return "", fmt.Errorf("build the CREATE DATABASE statement for %q: %w", name, err)
	}
	return stmt + ";", nil
}

// roleExistsOnTarget reports whether s3 has the role.
func (f *Fleet) roleExistsOnTarget(ctx context.Context, role string) (bool, error) {
	var ok bool
	if err := f.dst.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&ok); err != nil {
		return false, fmt.Errorf("look for the role %q on s3: %w", role, err)
	}
	return ok, nil
}

// createTarget creates the database on s3 with the encoding and collation of s1, and with its owner
// when s3 has that role.
func (f *Fleet) createTarget(ctx context.Context, d *Database) error {
	s, err := f.sourceSettings(ctx, d.Name)
	if err != nil {
		return err
	}
	withOwner, err := f.roleExistsOnTarget(ctx, s.Owner)
	if err != nil {
		return err
	}
	stmt, err := f.createDatabaseStatement(ctx, d.Name, s, withOwner)
	if err != nil {
		return err
	}
	d.Logger.Info("creating the database on s3", "encoding", s.Encoding, "collate", s.Collate, "ctype", s.Ctype, "owner_kept", withOwner)
	if _, err := f.dst.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("create database %q on s3: %w (the role needs CREATEDB or superuser, and s3 must have the locale %q; "+
			"or create the database yourself and set databases.create_target to false)", d.Name, err, s.Collate)
	}
	return nil
}

// ensurePublication creates the publication in the database on s1 when it is missing. The precheck has
// shown that the source role is a superuser and that no table lacks a replica identity: a publication
// makes UPDATE and DELETE fail on such a table.
func ensurePublication(ctx context.Context, d *Database) error {
	conn, err := pg.Connect(ctx, d.Cfg.Source.DSN, "gophant-publication")
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	name := d.Cfg.Source.PublicationName
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_publication WHERE pubname = $1)`, name).Scan(&exists); err != nil {
		return fmt.Errorf("look for the publication %q on s1: %w", name, err)
	}
	if exists {
		return nil
	}
	d.Logger.Info("creating the publication on s1", "publication", name)
	if _, err := conn.Exec(ctx, "CREATE PUBLICATION "+pg.Ident(name)+" FOR ALL TABLES WITH (publish_via_partition_root = true)"); err != nil {
		return fmt.Errorf("create the publication %q on s1: %w (the source role has to be a superuser; "+
			"or create it yourself and set databases.create_publication to false)", name, err)
	}
	return nil
}

// prepare makes sure that the database exists on s3 and has its publication on s1, as far as the
// configuration lets gophant create them. The precheck has passed for this database before.
func (f *Fleet) prepare(ctx context.Context, d *Database) error {
	onTarget, err := f.targetDatabases(ctx)
	if err != nil {
		return err
	}
	if !onTarget[d.Name] {
		if !f.Cfg.Databases.CreatesTarget() {
			return fmt.Errorf("database %q does not exist on s3 and databases.create_target is false", d.Name)
		}
		if err := f.createTarget(ctx, d); err != nil {
			return err
		}
	}
	if f.Cfg.Databases.CreatesPublication() {
		return ensurePublication(ctx, d)
	}
	return nil
}
