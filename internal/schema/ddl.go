// Package schema moves the table definitions from s1 to s3 with the PostgreSQL 18 pg_dump and
// pg_restore tools. The definitions are split in two sections: pre-data (tables, types, functions,
// sequences) is imported before the rows are copied, post-data (indexes, constraints, triggers,
// materialized view refreshes) afterwards, which makes the bulk load far faster.
package schema

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/pg"
)

// Section is a pg_dump section of a schema-only dump.
type Section string

// The two sections that are restored separately.
const (
	PreData  Section = "pre-data"
	PostData Section = "post-data"
)

// maxToolOutput bounds how much of the output of a failed tool is kept in an error message.
const maxToolOutput = 4096

// Runner runs pg_dump and pg_restore against s1 and s3.
type Runner struct {
	cfg    *config.Config
	logger *slog.Logger
}

// NewRunner returns a runner configured from cfg.
func NewRunner(cfg *config.Config, logger *slog.Logger) *Runner {
	return &Runner{cfg: cfg, logger: logger}
}

// Dump writes a schema-only archive (custom format) of s1 to file. One archive serves both
// restore steps, so pre-data and post-data come from the same catalog state.
func (r *Runner) Dump(ctx context.Context, file string) error {
	args, env, err := r.dumpArgs(file)
	if err != nil {
		return err
	}
	r.logger.Info("dumping the schema of s1", "tool", r.cfg.Tools.PgDumpBin)
	return r.run(ctx, r.cfg.Tools.PgDumpBin, args, env)
}

// RestorePreData creates the tables, types, functions and sequences on s3 in one transaction, so a
// failure leaves s3 empty and the step can simply be repeated.
//
// existingSchemas lists the schemas that s3 already has (for example public). An archive made with
// an explicit --schema=public contains CREATE SCHEMA public, which would abort the import, so the
// creation of those schemas is removed from the restore list; their contents are still imported.
func (r *Runner) RestorePreData(ctx context.Context, file string, existingSchemas []string) error {
	skip := make(map[string]bool, len(existingSchemas))
	for _, s := range existingSchemas {
		skip[s] = true
	}
	list, skipped, cleanup, err := r.restoreList(ctx, file, func(toc string) (string, []string) { return filterTOC(toc, skip) })
	if err != nil {
		return err
	}
	defer cleanup()
	if len(skipped) > 0 {
		r.logger.Info("schemas that already exist on s3 are not created again", "schemas", skipped)
	}

	args, env, err := r.restoreArgs(file, PreData, 0)
	if err != nil {
		return err
	}
	args = append([]string{"--use-list=" + list}, args...)
	r.logger.Info("importing pre-data on s3", "tool", r.cfg.Tools.PgRestoreBin)
	return r.run(ctx, r.cfg.Tools.PgRestoreBin, args, env)
}

// RestorePostData creates the indexes, constraints and triggers on s3 with the given parallelism.
//
// When a previous attempt may have created some of them (a restart, a failed attempt), existing lists
// what s3 already has: those entries are removed from the restore list and only the missing ones are
// created. Dropping and recreating is not an option, because an inherited constraint of a partition
// cannot be dropped on its own. Pass nil on a fresh target.
func (r *Runner) RestorePostData(ctx context.Context, file string, jobs int, existing *ExistingObjects) error {
	args, env, err := r.restoreArgs(file, PostData, jobs)
	if err != nil {
		return err
	}
	if existing != nil {
		list, skipped, cleanup, err := r.restoreList(ctx, file, func(toc string) (string, []string) { return filterPostData(toc, existing) })
		if err != nil {
			return err
		}
		defer cleanup()
		if len(skipped) > 0 {
			r.logger.Info("post-data objects that already exist on s3 are not created again", "objects", len(skipped))
		}
		args = append([]string{"--use-list=" + list}, args...)
	}
	r.logger.Info("importing post-data on s3", "tool", r.cfg.Tools.PgRestoreBin, "jobs", jobs, "skip_existing", existing != nil)
	return r.run(ctx, r.cfg.Tools.PgRestoreBin, args, env)
}

// ExistingObjects are the post-data objects that a database already has. Indexes are keyed by
// schema.name, constraints and triggers by schema.table.name.
type ExistingObjects struct {
	Indexes     map[string]bool
	Constraints map[string]bool
	Triggers    map[string]bool
}

// LoadExistingObjects reads the indexes, constraints and triggers of the database behind q.
func LoadExistingObjects(ctx context.Context, q pg.Querier) (*ExistingObjects, error) {
	load := func(sql string) (map[string]bool, error) {
		names, err := pg.QueryStrings(ctx, q, sql)
		if err != nil {
			return nil, err
		}
		set := make(map[string]bool, len(names))
		for _, n := range names {
			set[n] = true
		}
		return set, nil
	}
	var (
		ex  ExistingObjects
		err error
	)
	if ex.Indexes, err = load(`SELECT n.nspname || '.' || c.relname FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind IN ('i', 'I')`); err != nil {
		return nil, fmt.Errorf("list the indexes of s3: %w", err)
	}
	if ex.Constraints, err = load(`SELECT n.nspname || '.' || t.relname || '.' || con.conname FROM pg_constraint con
JOIN pg_class t ON t.oid = con.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace`); err != nil {
		return nil, fmt.Errorf("list the constraints of s3: %w", err)
	}
	if ex.Triggers, err = load(`SELECT n.nspname || '.' || t.relname || '.' || tg.tgname FROM pg_trigger tg
JOIN pg_class t ON t.oid = tg.tgrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE NOT tg.tgisinternal`); err != nil {
		return nil, fmt.Errorf("list the triggers of s3: %w", err)
	}
	return &ex, nil
}

// TempFile returns the path for the schema archive inside a fresh private directory and a cleanup
// function that removes the directory.
func TempFile() (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "gophant-schema-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir for the schema dump: %w", err)
	}
	return filepath.Join(dir, "schema.dump"), func() { _ = os.RemoveAll(dir) }, nil
}

// restoreList writes the table of contents of the archive to a temporary file after transform
// commented out the entries that must not be restored. It returns the file, the entries that were
// removed (as reported by transform) and a cleanup function.
func (r *Runner) restoreList(ctx context.Context, archive string, transform func(toc string) (string, []string)) (file string, skipped []string, cleanup func(), err error) {
	env, err := r.targetEnv()
	if err != nil {
		return "", nil, nil, err
	}
	var toc, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, r.cfg.Tools.PgRestoreBin, "--list", archive)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = &toc
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", nil, nil, fmt.Errorf("%s --list failed: %w\n%s", filepath.Base(r.cfg.Tools.PgRestoreBin), err, tail(stderr.String(), maxToolOutput))
	}
	filtered, skipped := transform(toc.String())

	f, err := os.CreateTemp("", "gophant-restore-list-*.txt")
	if err != nil {
		return "", nil, nil, fmt.Errorf("create the restore list: %w", err)
	}
	cleanup = func() { _ = os.Remove(f.Name()) }
	if _, err := f.WriteString(filtered); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, nil, fmt.Errorf("write the restore list: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, nil, fmt.Errorf("write the restore list: %w", err)
	}
	return f.Name(), skipped, cleanup, nil
}

// tocWords returns the words of a pg_restore --list entry after the dump id and the two OIDs, or nil
// for a comment or a malformed line. An entry looks like
//
//	3489; 2606 16466 CONSTRAINT public orders orders_pkey postgres
func tocWords(line string) []string {
	if strings.HasPrefix(line, ";") {
		return nil
	}
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return nil
	}
	return fields[3:]
}

// filterTOC comments out the "SCHEMA - <name>" entries of a pg_restore --list output for the schemas
// in skip. Entries of other types, such as the COMMENT or ACL of the schema, are kept: they apply to
// the schema that already exists.
func filterTOC(toc string, skip map[string]bool) (filtered string, skipped []string) {
	lines := strings.Split(toc, "\n")
	for i, line := range lines {
		w := tocWords(line)
		if len(w) >= 3 && w[0] == "SCHEMA" && w[1] == "-" && skip[w[2]] {
			lines[i] = ";" + line
			skipped = append(skipped, w[2])
		}
	}
	return strings.Join(lines, "\n"), skipped
}

// filterPostData comments out the INDEX, CONSTRAINT, FK CONSTRAINT and TRIGGER entries of objects that
// already exist. The other post-data entries (index attachments, rules, policies, materialized view
// refreshes) are kept: an index attachment can be repeated without harm.
func filterPostData(toc string, ex *ExistingObjects) (filtered string, skipped []string) {
	lines := strings.Split(toc, "\n")
	for i, line := range lines {
		w := tocWords(line)
		if len(w) == 0 {
			continue
		}
		var exists bool
		var name string
		switch {
		case w[0] == "INDEX" && len(w) >= 3 && w[1] != "ATTACH":
			name = w[1] + "." + w[2]
			exists = ex.Indexes[name]
		case w[0] == "CONSTRAINT" && len(w) >= 4:
			name = w[1] + "." + w[2] + "." + w[3]
			exists = ex.Constraints[name]
		case w[0] == "FK" && len(w) >= 5 && w[1] == "CONSTRAINT":
			name = w[2] + "." + w[3] + "." + w[4]
			exists = ex.Constraints[name]
		case w[0] == "TRIGGER" && len(w) >= 4:
			name = w[1] + "." + w[2] + "." + w[3]
			exists = ex.Triggers[name]
		}
		if exists {
			lines[i] = ";" + line
			skipped = append(skipped, name)
		}
	}
	return strings.Join(lines, "\n"), skipped
}

func (r *Runner) dumpArgs(file string) (args, env []string, err error) {
	dsn, password, err := pg.SplitPassword(r.cfg.Source.DSN)
	if err != nil {
		return nil, nil, fmt.Errorf("source.dsn: %w", err)
	}
	args = []string{
		"--schema-only",
		"--format=custom",
		"--no-publications",  // the migration publication belongs to s1 only
		"--no-subscriptions", // subscriptions of s1 must not be recreated on s3
		"--no-tablespaces",   // s3 has its own storage layout
		"--file=" + file,
		"--dbname=" + dsn,
	}
	for _, s := range r.cfg.Filter.IncludeSchemas {
		args = append(args, "--schema="+s)
	}
	for _, t := range r.cfg.Filter.ExcludeTables {
		args = append(args, "--exclude-table="+t)
	}
	return args, passwordEnv(password), nil
}

// targetEnv returns the environment that carries the target password.
func (r *Runner) targetEnv() ([]string, error) {
	_, password, err := pg.SplitPassword(r.cfg.Target.DSN)
	if err != nil {
		return nil, fmt.Errorf("target.dsn: %w", err)
	}
	return passwordEnv(password), nil
}

func (r *Runner) restoreArgs(file string, section Section, jobs int) (args, env []string, err error) {
	dsn, password, err := pg.SplitPassword(r.cfg.Target.DSN)
	if err != nil {
		return nil, nil, fmt.Errorf("target.dsn: %w", err)
	}
	args = []string{
		"--section=" + string(section),
		"--exit-on-error",
		"--dbname=" + dsn,
	}
	if section == PreData {
		args = append(args, "--single-transaction")
	}
	if jobs > 1 {
		args = append(args, "--jobs="+strconv.Itoa(jobs))
	}
	args = append(args, file)
	return args, passwordEnv(password), nil
}

func passwordEnv(password string) []string {
	if password == "" {
		return nil
	}
	return []string{"PGPASSWORD=" + password}
}

// run executes a tool and returns its tail of output in the error when it fails. The password is
// passed through the environment so it is never part of the command line or of an error message.
func (r *Runner) run(ctx context.Context, bin string, args, env []string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w\n%s", filepath.Base(bin), err, tail(out.String(), maxToolOutput))
	}
	if out.Len() > 0 {
		r.logger.Debug("tool output", "tool", filepath.Base(bin), "output", tail(out.String(), maxToolOutput))
	}
	return nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
