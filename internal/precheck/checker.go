package precheck

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shinession/gophant/internal/catalog"
	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/meta"
	"github.com/shinession/gophant/internal/pg"
)

// Checker runs every precheck rule against s1 and s3. It only reads from both databases.
type Checker struct {
	cfg    *config.Config
	src    *pgxpool.Pool
	dst    *pgxpool.Pool
	store  *meta.Store
	logger *slog.Logger
}

// New returns a checker. The pools are owned by the caller.
func New(cfg *config.Config, src, dst *pgxpool.Pool, logger *slog.Logger) *Checker {
	return &Checker{cfg: cfg, src: src, dst: dst, store: meta.NewStore(cfg.Target.MetaSchema), logger: logger}
}

// env is the data shared by the rules, read once at the start of a run.
type env struct {
	srcVersion, dstVersion int
	tables                 []catalog.Table
	fallbacks              []catalog.FormatFallback
	state                  *meta.SyncState
}

type rule struct {
	name string
	run  func(ctx context.Context, e *env) ([]Result, error)
}

// Run executes all rules and returns the report. An error is returned only when the shared data
// cannot be read at all; a rule that cannot run becomes a FAIL entry of the report.
func (c *Checker) Run(ctx context.Context) (*Report, error) {
	e, err := c.load(ctx)
	if err != nil {
		return nil, err
	}
	report := &Report{}
	for _, r := range c.rules() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		results, err := r.run(ctx, e)
		if err != nil {
			c.logger.Warn("precheck rule could not run", "rule", r.name, "error", err)
			report.Add(fail(r.name, "fix the connection or privilege problem and run precheck again", "the check could not run: %v", err))
			continue
		}
		report.Add(results...)
	}
	return report, nil
}

func (c *Checker) load(ctx context.Context) (*env, error) {
	e := &env{}
	var err error
	if e.srcVersion, err = pg.ServerVersionNum(ctx, c.src); err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	if e.dstVersion, err = pg.ServerVersionNum(ctx, c.dst); err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	if e.tables, err = catalog.ListTables(ctx, c.src, &c.cfg.Filter); err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	if err = catalog.LoadColumns(ctx, c.src, e.tables); err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	types, err := catalog.LoadUserTypes(ctx, c.src)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	e.fallbacks = catalog.AssignCopyFormats(e.tables, types)
	if e.state, err = c.store.GetState(ctx, c.dst); err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	return e, nil
}

func (c *Checker) rules() []rule {
	return []rule{
		{"versions", c.ruleVersions},
		{"source.wal_level", c.ruleWALLevel},
		{"source.privileges", c.rulePrivileges},
		{"source.max_slot_wal_keep_size", c.ruleSlotKeepSize},
		{"source.replication_capacity", c.ruleCapacity},
		{"source.publication", c.rulePublication},
		{"source.slot", c.ruleSlot},
		{"source.scope", c.ruleScope},
		{"source.replica_identity", pureRule(func(e *env) Result { return evalReplicaIdentity(e.tables) })},
		{"source.unlogged_tables", pureRule(func(e *env) Result { return evalUnlogged(e.tables) })},
		{"source.generated_columns", pureRule(func(e *env) Result { return evalGeneratedColumns(e.tables) })},
		{"source.copy_format", pureRule(func(e *env) Result { return evalBinaryCopy(e.fallbacks) })},
		{"source.monitoring", c.ruleMonitoring},
		{"source.large_objects", c.ruleLargeObjects},
		{"source.long_transactions", c.ruleLongTransactions},
		{"source.md5_passwords", c.ruleMD5},
		{"target.extensions", c.ruleExtensions},
		{"target.roles", c.ruleRoles},
		{"target.state", c.ruleTargetState},
		{"target.session_role", c.ruleTargetPrivileges},
		{"target.encoding", c.ruleEncoding},
		{"tools", c.ruleTools},
	}
}

func pureRule(f func(*env) Result) func(context.Context, *env) ([]Result, error) {
	return func(_ context.Context, e *env) ([]Result, error) { return []Result{f(e)}, nil }
}

func (c *Checker) ruleVersions(_ context.Context, e *env) ([]Result, error) {
	return evalVersions(e.srcVersion, e.dstVersion), nil
}

func (c *Checker) ruleWALLevel(ctx context.Context, _ *env) ([]Result, error) {
	var v string
	if err := c.src.QueryRow(ctx, `SELECT current_setting('wal_level')`).Scan(&v); err != nil {
		return nil, err
	}
	return []Result{evalWALLevel(v)}, nil
}

func (c *Checker) rulePrivileges(ctx context.Context, e *env) ([]Result, error) {
	var superuser, replication bool
	if err := c.src.QueryRow(ctx, `SELECT rolsuper, rolreplication FROM pg_roles WHERE rolname = current_user`).
		Scan(&superuser, &replication); err != nil {
		return nil, err
	}

	oids := make([]uint32, 0, len(e.tables))
	for _, t := range e.tables {
		oids = append(oids, t.OID)
	}
	noSelect, err := pg.QueryStrings(ctx, c.src, `
SELECT n.nspname || '.' || c.relname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.oid = ANY ($1::oid[]) AND NOT has_table_privilege(c.oid, 'SELECT')
ORDER BY 1`, oids)
	if err != nil {
		return nil, err
	}

	noSeq, err := pg.QueryStrings(ctx, c.src, `
SELECT n.nspname || '.' || c.relname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'S'
  AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_toast%'
  AND ($1::text[] IS NULL OR n.nspname = ANY ($1))
  -- SQL does not promise the order in which the conditions are evaluated, and
  -- has_sequence_privilege raises an error for anything that is not a sequence. CASE does keep the order.
  AND CASE WHEN c.relkind = 'S' THEN NOT has_sequence_privilege(c.oid, 'SELECT') ELSE false END
ORDER BY 1`, schemaFilterArg(c.cfg))
	if err != nil {
		return nil, err
	}
	return evalPrivileges(superuser, replication, noSelect, noSeq), nil
}

func (c *Checker) ruleSlotKeepSize(ctx context.Context, _ *env) ([]Result, error) {
	var mb int64
	if err := c.src.QueryRow(ctx, `SELECT setting::bigint FROM pg_settings WHERE name = 'max_slot_wal_keep_size'`).Scan(&mb); err != nil {
		return nil, err
	}
	return []Result{evalSlotKeepSize(mb, c.cfg.Precheck.AllowUnlimitedSlotWAL)}, nil
}

func (c *Checker) ruleCapacity(ctx context.Context, e *env) ([]Result, error) {
	var maxSlots, usedSlots, maxSenders, usedSenders int
	err := c.src.QueryRow(ctx, `SELECT current_setting('max_replication_slots')::int,
       (SELECT count(*) FROM pg_replication_slots)::int,
       current_setting('max_wal_senders')::int,
       (SELECT count(*) FROM pg_stat_replication)::int`).Scan(&maxSlots, &usedSlots, &maxSenders, &usedSenders)
	if err != nil {
		return nil, err
	}
	ownSlot := e.state != nil && e.state.SlotName == c.cfg.Source.SlotName
	return []Result{evalCapacity(maxSlots, usedSlots, maxSenders, usedSenders, ownSlot)}, nil
}

func (c *Checker) rulePublication(ctx context.Context, e *env) ([]Result, error) {
	var p publicationInfo
	err := c.src.QueryRow(ctx, `SELECT puballtables, pubinsert, pubupdate, pubdelete, pubtruncate, pubviaroot
FROM pg_publication WHERE pubname = $1`, c.cfg.Source.PublicationName).
		Scan(&p.AllTables, &p.Insert, &p.Update, &p.Delete, &p.Truncate, &p.ViaPartRoot)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return []Result{evalPublication(c.cfg.Source.PublicationName, p)}, nil
	case err != nil:
		return nil, err
	}
	p.Exists = true

	if !p.AllTables {
		published, err := pg.QueryStrings(ctx, c.src,
			`SELECT schemaname || '.' || tablename FROM pg_publication_tables WHERE pubname = $1`, c.cfg.Source.PublicationName)
		if err != nil {
			return nil, err
		}
		set := make(map[string]bool, len(published))
		for _, name := range published {
			set[name] = true
		}
		for _, t := range e.tables {
			if !t.IsPartition && !set[t.FullName()] {
				p.MissingTables = append(p.MissingTables, t.FullName())
			}
		}
	}
	return []Result{evalPublication(c.cfg.Source.PublicationName, p)}, nil
}

func (c *Checker) ruleSlot(ctx context.Context, e *env) ([]Result, error) {
	var s slotInfo
	err := c.src.QueryRow(ctx, `SELECT active, COALESCE(wal_status, ''), COALESCE(plugin, '')
FROM pg_replication_slots WHERE slot_name = $1`, c.cfg.Source.SlotName).Scan(&s.Active, &s.WALStatus, &s.Plugin)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		s.Exists = true
	}
	return []Result{evalSlot(c.cfg.Source.SlotName, s, e.state)}, nil
}

func (c *Checker) ruleScope(ctx context.Context, e *env) ([]Result, error) {
	var missing []string
	if len(c.cfg.Filter.IncludeSchemas) > 0 {
		found, err := pg.QueryStrings(ctx, c.src, `SELECT nspname FROM pg_namespace WHERE nspname = ANY ($1)`, c.cfg.Filter.IncludeSchemas)
		if err != nil {
			return nil, err
		}
		set := make(map[string]bool, len(found))
		for _, s := range found {
			set[s] = true
		}
		for _, s := range c.cfg.Filter.IncludeSchemas {
			if !set[s] {
				missing = append(missing, s)
			}
		}
	}
	return []Result{evalScope(e.tables, missing)}, nil
}

func (c *Checker) ruleMonitoring(ctx context.Context, _ *env) ([]Result, error) {
	var ok bool
	err := c.src.QueryRow(ctx, `SELECT pg_has_role(current_user, 'pg_read_all_stats', 'USAGE')
       OR COALESCE((SELECT rolsuper FROM pg_roles WHERE rolname = current_user), false)`).Scan(&ok)
	if err != nil {
		return nil, err
	}
	return []Result{evalMonitoring(ok)}, nil
}

func (c *Checker) ruleLargeObjects(ctx context.Context, _ *env) ([]Result, error) {
	var n int64
	if err := c.src.QueryRow(ctx, `SELECT count(*) FROM pg_largeobject_metadata`).Scan(&n); err != nil {
		return nil, err
	}
	return []Result{evalLargeObjects(n)}, nil
}

func (c *Checker) ruleLongTransactions(ctx context.Context, _ *env) ([]Result, error) {
	var n int64
	var oldest time.Duration
	err := c.src.QueryRow(ctx, `SELECT count(*), COALESCE(max(now() - xact_start), interval '0')
FROM pg_stat_activity
WHERE backend_xid IS NOT NULL AND xact_start < now() - interval '5 minutes' AND pid <> pg_backend_pid()`).Scan(&n, &oldest)
	if err != nil {
		return nil, err
	}
	return []Result{evalLongTransactions(n, oldest)}, nil
}

func (c *Checker) ruleMD5(ctx context.Context, _ *env) ([]Result, error) {
	roles, err := pg.QueryStrings(ctx, c.src, `SELECT rolname FROM pg_authid WHERE rolpassword LIKE 'md5%' ORDER BY rolname`)
	if err != nil {
		if pg.SQLState(err) == "42501" { // insufficient_privilege
			return []Result{evalMD5Roles(nil, true)}, nil
		}
		return nil, err
	}
	return []Result{evalMD5Roles(roles, false)}, nil
}

func (c *Checker) ruleExtensions(ctx context.Context, _ *env) ([]Result, error) {
	rows, err := c.src.Query(ctx, `SELECT e.extname, e.extversion, n.nspname
FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace ORDER BY e.extname`)
	if err != nil {
		return nil, err
	}
	var src []extInfo
	for rows.Next() {
		var x extInfo
		if err := rows.Scan(&x.Name, &x.Version, &x.Schema); err != nil {
			rows.Close()
			return nil, err
		}
		src = append(src, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	installed := map[string]string{}
	rows, err = c.dst.Query(ctx, `SELECT extname, extversion FROM pg_extension`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name, version string
		if err := rows.Scan(&name, &version); err != nil {
			rows.Close()
			return nil, err
		}
		installed[name] = version
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	availableNames, err := pg.QueryStrings(ctx, c.dst, `SELECT name FROM pg_available_extensions`)
	if err != nil {
		return nil, err
	}
	available := make(map[string]bool, len(availableNames))
	for _, n := range availableNames {
		available[n] = true
	}
	return []Result{evalExtensions(src, installed, available, &c.cfg.Filter, c.cfg.Snapshot.SkipDDL)}, nil
}

func (c *Checker) ruleRoles(ctx context.Context, e *env) ([]Result, error) {
	oids := make([]uint32, 0, len(e.tables))
	schemaSet := map[string]bool{}
	for _, t := range e.tables {
		oids = append(oids, t.OID)
		schemaSet[t.Schema] = true
	}
	needed, err := pg.QueryStrings(ctx, c.src, `
SELECT DISTINCT r.rolname
FROM (
    SELECT c.relowner AS role_oid FROM pg_class c WHERE c.oid = ANY ($1::oid[])
    UNION
    SELECT a.grantee FROM pg_class c, aclexplode(c.relacl) a WHERE c.oid = ANY ($1::oid[])
    UNION
    SELECT a.grantor FROM pg_class c, aclexplode(c.relacl) a WHERE c.oid = ANY ($1::oid[])
    UNION
    SELECT n.nspowner FROM pg_namespace n WHERE n.nspname = ANY ($2::text[])
    UNION
    SELECT p.proowner FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = ANY ($2::text[])
    UNION
    SELECT t.typowner FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
     WHERE n.nspname = ANY ($2::text[]) AND t.typtype IN ('e', 'd', 'c', 'r', 'm')
) x
JOIN pg_roles r ON r.oid = x.role_oid
ORDER BY 1`, oids, sortedKeys(schemaSet))
	if err != nil {
		return nil, err
	}
	existingNames, err := pg.QueryStrings(ctx, c.dst, `SELECT rolname FROM pg_roles WHERE rolname = ANY ($1)`, needed)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]bool, len(existingNames))
	for _, n := range existingNames {
		existing[n] = true
	}
	return []Result{evalRoles(needed, existing)}, nil
}

func (c *Checker) ruleTargetState(ctx context.Context, e *env) ([]Result, error) {
	names, err := pg.QueryStrings(ctx, c.dst, `
SELECT n.nspname || '.' || c.relname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p') AND n.nspname = ANY ($1::text[])`, tableSchemas(e.tables))
	if err != nil {
		return nil, err
	}
	exists := make(map[string]bool, len(names))
	for _, n := range names {
		exists[n] = true
	}

	// A schema prepared by hand (skip_ddl) must be empty. Once the migration has started the tables
	// belong to it and are no longer checked here.
	var nonEmpty []string
	if c.cfg.Snapshot.SkipDDL && (e.state == nil || e.state.Stage == meta.StagePrecheck) {
		for _, t := range e.tables {
			if !t.IsPhysical() || !exists[t.FullName()] {
				continue
			}
			var hasRows bool
			if err := c.dst.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM ONLY "+t.Quoted()+")").Scan(&hasRows); err != nil {
				return nil, fmt.Errorf("look for rows in %s on s3: %w", t.FullName(), err)
			}
			if hasRows {
				nonEmpty = append(nonEmpty, t.FullName())
			}
		}
	}
	return []Result{evalTargetState(e.tables, exists, c.cfg.Snapshot.SkipDDL, e.state, nonEmpty)}, nil
}

// ruleTargetPrivileges proves that the target role may use session_replication_role = replica,
// which both the COPY and the replay sessions need. The attempt runs in a rolled back transaction.
func (c *Checker) ruleTargetPrivileges(ctx context.Context, _ *env) ([]Result, error) {
	tx, err := c.dst.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := pg.SetReplicaRole(ctx, tx); err != nil {
		return []Result{fail("target.session_role",
			"use a superuser on s3, or GRANT SET ON PARAMETER session_replication_role TO <role>",
			"the target role cannot SET session_replication_role: %v", err)}, nil
	}
	return []Result{pass("target.session_role", "the target role may SET session_replication_role = replica")}, nil
}

func (c *Checker) ruleEncoding(ctx context.Context, _ *env) ([]Result, error) {
	const q = `SELECT pg_encoding_to_char(encoding), datcollate, datctype FROM pg_database WHERE datname = current_database()`
	var src, dst dbEncoding
	if err := c.src.QueryRow(ctx, q).Scan(&src.Encoding, &src.Collate, &src.Ctype); err != nil {
		return nil, err
	}
	if err := c.dst.QueryRow(ctx, q).Scan(&dst.Encoding, &dst.Collate, &dst.Ctype); err != nil {
		return nil, err
	}
	return []Result{evalEncoding(src, dst)}, nil
}

func (c *Checker) ruleTools(ctx context.Context, e *env) ([]Result, error) {
	var out []Result
	for _, t := range []struct{ name, bin string }{
		{"pg_dump", c.cfg.Tools.PgDumpBin},
		{"pg_restore", c.cfg.Tools.PgRestoreBin},
	} {
		v, err := ToolMajorVersion(ctx, t.bin)
		out = append(out, evalTool(t.name, v, major(e.srcVersion), err))
	}
	return out, nil
}

var toolVersionPattern = regexp.MustCompile(`\(PostgreSQL\)\s+(\d+)`)

// ToolMajorVersion runs "<bin> --version" and returns the PostgreSQL major version it reports.
func ToolMajorVersion(ctx context.Context, bin string) (int, error) {
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return 0, fmt.Errorf("cannot run %q: %w", bin, err)
	}
	return parseToolMajor(string(out))
}

func parseToolMajor(output string) (int, error) {
	m := toolVersionPattern.FindStringSubmatch(output)
	if m == nil {
		return 0, fmt.Errorf("unexpected version output %q", output)
	}
	return strconv.Atoi(m[1])
}

// schemaFilterArg returns the include_schemas list as a query argument, or NULL when every schema
// is in scope.
func schemaFilterArg(cfg *config.Config) any {
	if len(cfg.Filter.IncludeSchemas) == 0 {
		return nil
	}
	return cfg.Filter.IncludeSchemas
}

func tableSchemas(tables []catalog.Table) []string {
	set := map[string]bool{}
	for _, t := range tables {
		set[t.Schema] = true
	}
	return sortedKeys(set)
}
