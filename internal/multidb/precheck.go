package multidb

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shinession/gophant/internal/format"
	"github.com/shinession/gophant/internal/pg"
	"github.com/shinession/gophant/internal/precheck"
)

// precheckParallel is the number of databases checked at the same time. The checks only read.
const precheckParallel = 4

// clusterChecks are the checks whose answer is the same for every database of the cluster: the
// settings of the servers, the role, the tools. The multi-database report shows them once. Every
// other check belongs to one database. target.roles is special: each database needs some roles and
// the report merges them into one list.
var clusterChecks = map[string]bool{
	"source.version":                true,
	"target.version":                true,
	"source.wal_level":              true,
	"source.replication_role":       true,
	"source.max_slot_wal_keep_size": true,
	"source.replication_capacity":   true,
	"source.monitoring":             true,
	"source.long_transactions":      true,
	"source.md5_passwords":          true,
	"target.roles":                  true,
	"target.session_role":           true,
	"tools.pg_dump":                 true,
	"tools.pg_restore":              true,
}

// DatabaseReport is the precheck of one database.
type DatabaseReport struct {
	Database *Database
	// OnTarget says whether s3 has the database already.
	OnTarget bool
	// Results are the checks that belong to this database. The cluster-wide ones are in Report.Cluster.
	Results []precheck.Result
	Tables  int
	Bytes   int64
	// Skipped is the reason why the database has nothing to migrate; it is then not checked further.
	Skipped string
}

// problems counts the warnings and the failures of the database.
func (d *DatabaseReport) problems() (warn, fail int) {
	for _, r := range d.Results {
		switch r.Severity {
		case precheck.SeverityWarn:
			warn++
		case precheck.SeverityFail:
			fail++
		}
	}
	return warn, fail
}

// Report is the precheck of every selected database.
type Report struct {
	// Cluster are the checks that apply to the whole cluster, each shown once.
	Cluster   []precheck.Result
	Databases []*DatabaseReport
	// LeftOut are the databases of s1 that the configuration left out.
	LeftOut []string
	// CreatesTarget tells whether a database that s3 does not have is created by the snapshot stage.
	CreatesTarget bool
}

// ClusterFailed reports whether a cluster-wide check failed: then no database can start.
func (r *Report) ClusterFailed() bool {
	return slices.ContainsFunc(r.Cluster, func(res precheck.Result) bool { return res.Severity == precheck.SeverityFail })
}

// Blocked returns the databases that have a failure of their own.
func (r *Report) Blocked() []*DatabaseReport {
	return r.filter(func(d *DatabaseReport) bool { _, fail := d.problems(); return d.Skipped == "" && fail > 0 })
}

// Ready returns the databases that have no failure, as far as the cluster allows it.
func (r *Report) Ready() []*DatabaseReport {
	if r.ClusterFailed() {
		return nil
	}
	return r.filter(func(d *DatabaseReport) bool { _, fail := d.problems(); return d.Skipped == "" && fail == 0 })
}

// Skipped returns the databases that have nothing to migrate.
func (r *Report) Skipped() []*DatabaseReport {
	return r.filter(func(d *DatabaseReport) bool { return d.Skipped != "" })
}

// Failed reports whether the precheck found a failure anywhere that matters.
func (r *Report) Failed() bool { return r.ClusterFailed() || len(r.Blocked()) > 0 }

// FailureCount returns the number of failed checks of the cluster and of the databases that are not skipped.
func (r *Report) FailureCount() int {
	n := 0
	for _, res := range r.Cluster {
		if res.Severity == precheck.SeverityFail {
			n++
		}
	}
	for _, d := range r.Databases {
		if d.Skipped == "" {
			_, fail := d.problems()
			n += fail
		}
	}
	return n
}

func (r *Report) filter(keep func(*DatabaseReport) bool) []*DatabaseReport {
	var out []*DatabaseReport
	for _, d := range r.Databases {
		if keep(d) {
			out = append(out, d)
		}
	}
	return out
}

// Precheck checks the databases in dbs and, once, what applies to the whole cluster. owners are the
// databases that need a slot and a walsender in this run: the capacity of s1 has to cover the ones
// among them that have something to migrate, also the ones that are not checked here because they
// stream already.
func (f *Fleet) Precheck(ctx context.Context, dbs, owners []*Database) (*Report, error) {
	onTarget, err := f.targetDatabases(ctx)
	if err != nil {
		return nil, err
	}

	reports := make(map[string]*DatabaseReport, len(dbs))
	var mu sync.Mutex
	forEach(ctx, dbs, precheckParallel, func(ctx context.Context, d *Database) (string, error) {
		rep := f.precheckOne(ctx, d, onTarget[d.Name])
		mu.Lock()
		reports[d.Name] = rep
		mu.Unlock()
		return "", nil
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	out := &Report{LeftOut: f.LeftOut, CreatesTarget: f.Cfg.Databases.CreatesTarget()}
	for _, d := range dbs {
		out.Databases = append(out.Databases, reports[d.Name])
	}
	out.Cluster = replaceCheck(splitCluster(out.Databases), f.capacityResult(ctx, owners, out.Databases))
	return out, nil
}

// capacityResult checks the slots and the walsenders of s1 for the databases of owners that are not
// skipped: a database without tables needs neither.
func (f *Fleet) capacityResult(ctx context.Context, owners []*Database, reports []*DatabaseReport) precheck.Result {
	skipped := map[string]bool{}
	for _, r := range reports {
		if r.Skipped != "" {
			skipped[r.Database.Name] = true
		}
	}
	var slots []string
	for _, d := range owners {
		if !skipped[d.Name] {
			slots = append(slots, d.Cfg.Source.SlotName)
		}
	}
	res, err := precheck.CheckCapacity(ctx, f.src, slots)
	if err != nil {
		return precheck.Result{ID: "source.replication_capacity", Severity: precheck.SeverityFail,
			Summary: fmt.Sprintf("the check could not run: %v", err), Hint: "fix the connection or privilege problem and run precheck again"}
	}
	return res
}

// replaceCheck puts r in the list in place of the check with the same ID, or at the end.
func replaceCheck(results []precheck.Result, r precheck.Result) []precheck.Result {
	if i := slices.IndexFunc(results, func(x precheck.Result) bool { return x.ID == r.ID }); i >= 0 {
		results[i] = r
		return results
	}
	return append(results, r)
}

// precheckOne checks one database. A problem with the connections becomes a failed check of the
// database instead of an error, so one unreachable database does not hide the others.
func (f *Fleet) precheckOne(ctx context.Context, d *Database, onTarget bool) *DatabaseReport {
	rep := &DatabaseReport{Database: d, OnTarget: onTarget}
	failed := func(id, hint, format string, args ...any) *DatabaseReport {
		rep.Results = append(rep.Results, precheck.Result{
			ID: id, Severity: precheck.SeverityFail, Summary: fmt.Sprintf(format, args...), Hint: hint,
		})
		return rep
	}
	if !d.CanConnect {
		return failed("source.connect", "GRANT CONNECT ON DATABASE "+pg.Ident(d.Name)+" TO <role>, or add the database to databases.exclude",
			"the source role may not connect to database %s", d.Name)
	}
	src, err := pg.NewPool(ctx, d.Cfg.Source.DSN, "gophant-precheck", 2)
	if err != nil {
		return failed("source.connect", "fix the connection or the pg_hba.conf of s1 for this database", "cannot connect to the database on s1: %v", err)
	}
	defer src.Close()

	var dst *pgxpool.Pool
	if onTarget {
		if dst, err = pg.NewPool(ctx, d.Cfg.Target.DSN, "gophant-precheck", 2); err != nil {
			return failed("target.connect", "fix the connection or the pg_hba.conf of s3 for this database", "cannot connect to the database on s3: %v", err)
		}
		defer dst.Close()
	} else {
		dst = f.dst // the database does not exist yet; only the checks that need no database use it
	}

	opts := precheck.Options{TargetMissing: !onTarget, CreatePublication: f.Cfg.Databases.CreatesPublication()}
	report, err := precheck.NewWithOptions(d.Cfg, src, dst, d.Logger, opts).Run(ctx)
	if err != nil {
		return failed("precheck", "fix the connection or privilege problem and run precheck again", "the checks could not run: %v", err)
	}
	rep.Results, rep.Tables, rep.Bytes = report.Results, report.Tables, report.Bytes
	if report.Tables == 0 {
		rep.Skipped = skipReasonEmpty
		return rep
	}
	if !onTarget {
		rep.Results = append(rep.Results, f.targetDatabaseResult(ctx, d))
	}
	return rep
}

// targetDatabaseResult says what happens to a database that s3 does not have yet.
func (f *Fleet) targetDatabaseResult(ctx context.Context, d *Database) precheck.Result {
	const id = "target.database"
	fail := func(hint, fix, format string, args ...any) precheck.Result {
		return precheck.Result{ID: id, Severity: precheck.SeverityFail, Summary: fmt.Sprintf(format, args...), Hint: hint, Fix: fix}
	}
	if f.Cfg.Snapshot.SkipDDL {
		return fail("create the database and its schema on s3, or set snapshot.skip_ddl to false", "",
			"the database does not exist on s3 and snapshot.skip_ddl is true")
	}
	s, err := f.sourceSettings(ctx, d.Name)
	if err != nil {
		return fail("the settings of the database on s1 could not be read", "", "%v", err)
	}
	withOwner, err := f.roleExistsOnTarget(ctx, s.Owner)
	if err != nil {
		return fail("the role could not be looked up on s3", "", "%v", err)
	}
	if !f.Cfg.Databases.CreatesTarget() {
		stmt, err := f.createDatabaseStatement(ctx, d.Name, s, withOwner)
		if err != nil {
			return fail("the CREATE DATABASE statement could not be built", "", "%v", err)
		}
		return fail("create it with the same encoding and collation, or set databases.create_target to true", stmt,
			"the database does not exist on s3 and databases.create_target is false")
	}
	if !withOwner {
		return precheck.Result{ID: id, Severity: precheck.SeverityWarn,
			Summary: fmt.Sprintf("the database does not exist on s3, the snapshot stage creates it (encoding %s, collation %s); its owner %s does not exist on s3, so the connecting role owns it",
				s.Encoding, s.Collate, s.Owner),
			Hint: "create the role on s3 first if the database should have the same owner"}
	}
	return precheck.Result{ID: id, Severity: precheck.SeverityPass,
		Summary: fmt.Sprintf("the database does not exist on s3, the snapshot stage creates it (encoding %s, collation %s, owner %s)", s.Encoding, s.Collate, s.Owner)}
}

// splitCluster moves the cluster-wide checks out of the reports of the databases and returns them
// once. When the databases disagree the worst answer is kept; target.roles is merged into one list of
// the missing roles. A database that is skipped does not contribute: nothing of it is migrated.
func splitCluster(reports []*DatabaseReport) []precheck.Result {
	var order []string
	best := map[string]precheck.Result{}
	var rolesSeen bool
	var rolesHint string
	missingRoles := map[string]bool{}

	for _, rep := range reports {
		var own []precheck.Result
		for _, res := range rep.Results {
			switch {
			case !clusterChecks[res.ID]:
				own = append(own, res)
			case rep.Skipped != "":
				// dropped
			case res.ID == "target.roles":
				rolesSeen = true
				if res.Severity == precheck.SeverityFail {
					rolesHint = res.Hint
					for _, role := range res.Items {
						missingRoles[role] = true
					}
				}
			default:
				cur, ok := best[res.ID]
				switch {
				case !ok:
					order = append(order, res.ID)
					best[res.ID] = res
				case res.Severity > cur.Severity:
					best[res.ID] = res
				}
			}
		}
		rep.Results = own
	}

	out := make([]precheck.Result, 0, len(order)+1)
	for _, id := range order {
		out = append(out, best[id])
	}
	switch {
	case len(missingRoles) > 0:
		roles := make([]string, 0, len(missingRoles))
		for role := range missingRoles {
			roles = append(roles, role)
		}
		slices.Sort(roles)
		out = append(out, precheck.Result{
			ID: "target.roles", Severity: precheck.SeverityFail, Items: roles, Hint: rolesHint,
			Summary: fmt.Sprintf("%d role(s) used by the objects of the databases do not exist on s3", len(roles)),
		})
	case rolesSeen:
		out = append(out, precheck.Result{
			ID: "target.roles", Severity: precheck.SeverityPass,
			Summary: "every role that owns or is granted objects in the databases exists on s3",
		})
	}
	return out
}

// Print writes the report: the cluster checks, a table of the databases, the problems of each
// database and the statements that fix what gophant is not allowed to fix itself.
func (r *Report) Print(w io.Writer) {
	title := fmt.Sprintf("gophant precheck report (%d database(s))", len(r.Databases))
	fmt.Fprintln(w, title)
	fmt.Fprintln(w, strings.Repeat("=", len(title)))
	if len(r.LeftOut) > 0 {
		fmt.Fprintf(w, "left out by databases.include, databases.exclude or --database: %s\n", strings.Join(r.LeftOut, ", "))
	}

	fmt.Fprintln(w, "\nCluster")
	precheck.PrintResults(w, r.Cluster, false)

	fmt.Fprintln(w, "\nDatabases")
	r.printTable(w)

	for _, d := range r.Databases {
		warn, fail := d.problems()
		if d.Skipped != "" || warn+fail == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s: %d warning(s), %d failure(s)\n", d.Database.Name, warn, fail)
		precheck.PrintResults(w, d.Results, true)
	}
	r.printScripts(w)

	// "ready" means that the database has no failure of its own; a failure of the cluster is reported below.
	blocked, skipped := len(r.Blocked()), len(r.Skipped())
	fmt.Fprintf(w, "\nSummary: %d database(s): %d ready, %d blocked, %d skipped\n", len(r.Databases), len(r.Databases)-blocked-skipped, blocked, skipped)
	switch {
	case r.ClusterFailed():
		fmt.Fprintln(w, "Result: BLOCKED - fix the failures of the cluster above, no database can start.")
	case blocked > 0:
		fmt.Fprintln(w, "Result: BLOCKED - fix the failures above. The databases that are ready can start without them: gophant run and gophant snapshot ask before they skip the blocked ones.")
	default:
		fmt.Fprintln(w, "Result: OK - the migration can start.")
	}
}

func (r *Report) printTable(w io.Writer) {
	width := len("DATABASE")
	for _, d := range r.Databases {
		width = max(width, len(d.Database.Name))
	}
	fmt.Fprintf(w, "  %-*s  %6s  %10s  %-15s  %s\n", width, "DATABASE", "TABLES", "SIZE", "ON S3", "RESULT")
	for _, d := range r.Databases {
		onTarget := "exists"
		switch {
		case d.OnTarget:
		case d.Skipped != "":
			onTarget = "-" // nothing is created for a database that is skipped
		case r.CreatesTarget:
			onTarget = "will be created"
		default:
			onTarget = "missing"
		}
		// A database whose checks could not run, or that has no table, has no numbers to show.
		size, tables := "-", "-"
		if d.Tables > 0 {
			size, tables = format.Bytes(d.Bytes), format.Int(int64(d.Tables))
		}
		warn, fail := d.problems()
		var result string
		switch {
		case d.Skipped != "":
			result = "skipped: " + d.Skipped
		case fail > 0:
			result = fmt.Sprintf("BLOCKED, %d failure(s)", fail)
		case warn > 0:
			result = fmt.Sprintf("ready, %d warning(s)", warn)
		default:
			result = "ready"
		}
		fmt.Fprintf(w, "  %-*s  %6s  %10s  %-15s  %s\n", width, d.Database.Name, tables, size, onTarget, result)
	}
}

// printScripts prints the statements that the failed checks offer, grouped by the server they run on.
// A publication is only offered for a database without a failed replica identity check: as soon as
// the publication exists, UPDATE and DELETE on a table without usable identity fail on s1.
func (r *Report) printScripts(w io.Writer) {
	var onS1, onS3 []string
	for _, d := range r.Databases {
		if d.Skipped != "" {
			continue
		}
		identityBroken := slices.ContainsFunc(d.Results, func(res precheck.Result) bool {
			return res.ID == "source.replica_identity" && res.Severity == precheck.SeverityFail
		})
		for _, res := range d.Results {
			if res.Fix == "" {
				continue
			}
			switch res.ID {
			case "source.publication":
				if identityBroken {
					onS1 = append(onS1, fmt.Sprintf("-- %s: fix source.replica_identity first, then run:", d.Database.Name),
						"--   "+res.Fix)
					continue
				}
				onS1 = append(onS1, psqlConnect(d.Database.Name), res.Fix)
			case "target.database":
				onS3 = append(onS3, res.Fix)
			}
		}
	}
	if len(onS1) > 0 {
		fmt.Fprintln(w, "\nTo create the publications yourself, run as a superuser on s1 (psql):")
		for _, line := range onS1 {
			fmt.Fprintln(w, "  "+line)
		}
	}
	if len(onS3) > 0 {
		fmt.Fprintln(w, "\nTo create the databases yourself, run as a role with CREATEDB on s3:")
		for _, line := range onS3 {
			fmt.Fprintln(w, "  "+line)
		}
	}
}

// psqlConnect returns the psql meta-command that switches to a database; a double quote in the name is doubled.
func psqlConnect(name string) string {
	return `\connect "` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
