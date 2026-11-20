package multidb

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/shinession/gophant/internal/precheck"
)

func res(id string, sev precheck.Severity, summary string) precheck.Result {
	return precheck.Result{ID: id, Severity: sev, Summary: summary}
}

func dbReport(name string, tables int, results ...precheck.Result) *DatabaseReport {
	return &DatabaseReport{
		Database: &Database{Name: name}, OnTarget: true, Results: results, Tables: tables, Bytes: int64(tables) * 1024,
	}
}

func ids(results []precheck.Result) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = r.ID
	}
	return out
}

func TestSplitClusterShowsTheClusterChecksOnce(t *testing.T) {
	a := dbReport("a", 3,
		res("source.wal_level", precheck.SeverityPass, "wal_level = logical"),
		res("source.scope", precheck.SeverityPass, "3 table(s)"),
		res("tools.pg_dump", precheck.SeverityPass, "pg_dump is version 18"))
	b := dbReport("b", 5,
		res("source.wal_level", precheck.SeverityPass, "wal_level = logical"),
		res("source.scope", precheck.SeverityPass, "5 table(s)"),
		res("tools.pg_dump", precheck.SeverityPass, "pg_dump is version 18"))
	cluster := splitCluster([]*DatabaseReport{a, b})

	if got := ids(cluster); !slices.Equal(got, []string{"source.wal_level", "tools.pg_dump"}) {
		t.Errorf("cluster checks = %v", got)
	}
	for _, d := range []*DatabaseReport{a, b} {
		if got := ids(d.Results); !slices.Equal(got, []string{"source.scope"}) {
			t.Errorf("%s keeps %v, want only its own check", d.Database.Name, got)
		}
	}
}

func TestSplitClusterKeepsTheWorstAnswer(t *testing.T) {
	a := dbReport("a", 1, res("source.replication_capacity", precheck.SeverityPass, "slots 0/8 used"))
	b := dbReport("b", 1, res("source.replication_capacity", precheck.SeverityFail, "no free slot"))
	c := dbReport("c", 1, res("source.replication_capacity", precheck.SeverityWarn, "tight"))
	cluster := splitCluster([]*DatabaseReport{a, b, c})
	if len(cluster) != 1 || cluster[0].Severity != precheck.SeverityFail || cluster[0].Summary != "no free slot" {
		t.Errorf("cluster = %+v", cluster)
	}
}

func TestSplitClusterMergesTheMissingRoles(t *testing.T) {
	missing := func(roles ...string) precheck.Result {
		return precheck.Result{ID: "target.roles", Severity: precheck.SeverityFail, Items: roles, Hint: "create the roles first",
			Summary: "roles missing"}
	}
	a := dbReport("a", 1, missing("owner_a", "shared"))
	b := dbReport("b", 1, missing("shared", "owner_b"))
	ok := dbReport("c", 1, res("target.roles", precheck.SeverityPass, "all 2 role(s) exist"))
	cluster := splitCluster([]*DatabaseReport{a, b, ok})

	if len(cluster) != 1 || cluster[0].ID != "target.roles" || cluster[0].Severity != precheck.SeverityFail {
		t.Fatalf("cluster = %+v", cluster)
	}
	if want := []string{"owner_a", "owner_b", "shared"}; !slices.Equal(cluster[0].Items, want) {
		t.Errorf("roles = %v, want %v", cluster[0].Items, want)
	}
	if !strings.Contains(cluster[0].Summary, "3 role(s)") || cluster[0].Hint != "create the roles first" {
		t.Errorf("summary or hint wrong: %+v", cluster[0])
	}

	// With nothing missing the merged result passes.
	cluster = splitCluster([]*DatabaseReport{ok, dbReport("d", 1, res("target.roles", precheck.SeverityPass, "fine"))})
	if len(cluster) != 1 || cluster[0].Severity != precheck.SeverityPass {
		t.Errorf("cluster = %+v", cluster)
	}
}

// A database without tables is not migrated, so the roles its objects need must not block the others.
func TestSplitClusterIgnoresASkippedDatabase(t *testing.T) {
	skipped := dbReport("empty", 0, precheck.Result{ID: "target.roles", Severity: precheck.SeverityFail, Items: []string{"nobody_needs_me"}})
	skipped.Skipped = "no table in scope, nothing to copy"
	live := dbReport("live", 2, res("target.roles", precheck.SeverityPass, "fine"))
	cluster := splitCluster([]*DatabaseReport{skipped, live})
	if len(cluster) != 1 || cluster[0].Severity != precheck.SeverityPass {
		t.Errorf("cluster = %+v", cluster)
	}
}

func TestReplaceCheck(t *testing.T) {
	in := []precheck.Result{
		res("a", precheck.SeverityPass, "first"),
		res("source.replication_capacity", precheck.SeverityPass, "old"),
		res("c", precheck.SeverityPass, "last"),
	}
	got := replaceCheck(in, res("source.replication_capacity", precheck.SeverityFail, "new"))
	if want := []string{"a", "source.replication_capacity", "c"}; !slices.Equal(ids(got), want) || got[1].Summary != "new" {
		t.Errorf("a check is replaced where it stands: %v %+v", ids(got), got[1])
	}
	got = replaceCheck(got, res("extra", precheck.SeverityPass, "appended"))
	if want := []string{"a", "source.replication_capacity", "c", "extra"}; !slices.Equal(ids(got), want) {
		t.Errorf("a missing check is appended: %v", ids(got))
	}
}

func TestReportClassifiesTheDatabases(t *testing.T) {
	ready := dbReport("ready", 2, res("source.scope", precheck.SeverityPass, "ok"))
	warned := dbReport("warned", 2, res("source.generated_columns", precheck.SeverityWarn, "generated"))
	blocked := dbReport("blocked", 2, res("source.replica_identity", precheck.SeverityFail, "no key"))
	empty := dbReport("empty", 0)
	empty.Skipped = "no table in scope, nothing to copy"
	r := &Report{Databases: []*DatabaseReport{ready, warned, blocked, empty}}

	names := func(ds []*DatabaseReport) []string {
		var out []string
		for _, d := range ds {
			out = append(out, d.Database.Name)
		}
		return out
	}
	if got := names(r.Ready()); !slices.Equal(got, []string{"ready", "warned"}) {
		t.Errorf("ready = %v", got)
	}
	if got := names(r.Blocked()); !slices.Equal(got, []string{"blocked"}) {
		t.Errorf("blocked = %v", got)
	}
	if got := names(r.Skipped()); !slices.Equal(got, []string{"empty"}) {
		t.Errorf("skipped = %v", got)
	}
	if !r.Failed() || r.ClusterFailed() || r.FailureCount() != 1 {
		t.Errorf("failed=%v cluster=%v failures=%d", r.Failed(), r.ClusterFailed(), r.FailureCount())
	}

	// A failure of the cluster blocks everything.
	r.Cluster = []precheck.Result{res("source.wal_level", precheck.SeverityFail, "wal_level = replica")}
	if len(r.Ready()) != 0 || !r.ClusterFailed() || r.FailureCount() != 2 {
		t.Errorf("ready=%d cluster=%v failures=%d", len(r.Ready()), r.ClusterFailed(), r.FailureCount())
	}
}

func TestReportWithOnlyWarningsDoesNotFail(t *testing.T) {
	r := &Report{
		Cluster:   []precheck.Result{res("source.md5_passwords", precheck.SeverityWarn, "md5")},
		Databases: []*DatabaseReport{dbReport("a", 1, res("source.unlogged_tables", precheck.SeverityWarn, "unlogged"))},
	}
	if r.Failed() || r.FailureCount() != 0 || len(r.Ready()) != 1 {
		t.Errorf("failed=%v failures=%d ready=%d", r.Failed(), r.FailureCount(), len(r.Ready()))
	}
}

func TestReportPrint(t *testing.T) {
	good := dbReport("appdb", 41, res("source.scope", precheck.SeverityPass, "41 table(s)"))
	bad := dbReport("Sales-2024", 12,
		res("source.scope", precheck.SeverityPass, "12 table(s)"),
		precheck.Result{ID: "source.replica_identity", Severity: precheck.SeverityFail, Summary: "1 table(s) cannot replicate UPDATE/DELETE",
			Items: []string{"public.events: no primary key"}, Hint: "add a primary key"},
		precheck.Result{ID: "source.publication", Severity: precheck.SeverityFail, Summary: "publication gophant_pub does not exist",
			Fix: `CREATE PUBLICATION "gophant_pub" FOR ALL TABLES WITH (publish_via_partition_root = true);`})
	bad.OnTarget = false
	empty := dbReport("scratch", 0)
	empty.Skipped = "no table in scope, nothing to copy"
	emptyMissing := dbReport("unused", 0)
	emptyMissing.OnTarget = false
	emptyMissing.Skipped = "no table in scope, nothing to copy"
	r := &Report{
		Cluster:       []precheck.Result{res("source.wal_level", precheck.SeverityPass, "wal_level = logical")},
		Databases:     []*DatabaseReport{good, bad, empty, emptyMissing},
		LeftOut:       []string{"postgres"},
		CreatesTarget: true,
	}

	var buf bytes.Buffer
	r.Print(&buf)
	out := buf.String()
	for _, want := range []string{
		"gophant precheck report (4 database(s))",
		"left out by databases.include, databases.exclude or --database: postgres",
		"Cluster\n[PASS] source.wal_level",
		"appdb", "ready",
		"Sales-2024", "will be created", "BLOCKED, 2 failure(s)",
		"scratch", "skipped: no table in scope, nothing to copy",
		"Sales-2024: 0 warning(s), 2 failure(s)",
		"[FAIL] source.replica_identity", "- public.events: no primary key", "hint: add a primary key",
		"Summary: 4 database(s): 1 ready, 1 blocked, 2 skipped",
		"Result: BLOCKED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report misses %q:\n%s", want, out)
		}
	}
	// Nothing is created for a database that is skipped, so the table must not promise it.
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "unused") && strings.Contains(line, "will be created") {
			t.Errorf("a skipped database is not created on s3: %q", line)
		}
	}
	// The passing checks of a database are not repeated, and the script must hold back the publication
	// of a database whose replica identity has to be fixed first.
	if strings.Contains(out, "[PASS] source.scope") {
		t.Errorf("a passing check of a database must not be listed:\n%s", out)
	}
	if !strings.Contains(out, "-- Sales-2024: fix source.replica_identity first, then run:") {
		t.Errorf("the publication must be held back:\n%s", out)
	}
	if strings.Contains(out, "\n  CREATE PUBLICATION") {
		t.Errorf("the publication statement must be commented out:\n%s", out)
	}
}

func TestReportPrintOK(t *testing.T) {
	r := &Report{
		Cluster:   []precheck.Result{res("source.wal_level", precheck.SeverityPass, "wal_level = logical")},
		Databases: []*DatabaseReport{dbReport("appdb", 3, res("source.scope", precheck.SeverityPass, "3 table(s)"))},
	}
	var buf bytes.Buffer
	r.Print(&buf)
	if !strings.Contains(buf.String(), "Result: OK - the migration can start.") || !strings.Contains(buf.String(), "1 ready, 0 blocked, 0 skipped") {
		t.Errorf("unexpected report:\n%s", buf.String())
	}
}

func TestReportPrintScripts(t *testing.T) {
	pub := dbReport("crm", 2, precheck.Result{ID: "source.publication", Severity: precheck.SeverityFail, Summary: "missing",
		Fix: `CREATE PUBLICATION "gophant_pub" FOR ALL TABLES WITH (publish_via_partition_root = true);`})
	quoted := dbReport(`odd"name`, 2, precheck.Result{ID: "source.publication", Severity: precheck.SeverityFail, Summary: "missing", Fix: "CREATE PUBLICATION x;"})
	missingDB := dbReport("new", 2, precheck.Result{ID: "target.database", Severity: precheck.SeverityFail, Summary: "missing",
		Fix: `CREATE DATABASE "new" TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C';`})
	missingDB.OnTarget = false
	r := &Report{Databases: []*DatabaseReport{pub, quoted, missingDB}}

	var buf bytes.Buffer
	r.printScripts(&buf)
	out := buf.String()
	for _, want := range []string{
		"run as a superuser on s1",
		"  \\connect \"crm\"\n  CREATE PUBLICATION \"gophant_pub\"",
		`  \connect "odd""name"`,
		"run as a role with CREATEDB on s3",
		`  CREATE DATABASE "new" TEMPLATE template0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the scripts miss %q:\n%s", want, out)
		}
	}

	buf.Reset()
	(&Report{Databases: []*DatabaseReport{dbReport("fine", 1)}}).printScripts(&buf)
	if buf.Len() != 0 {
		t.Errorf("nothing to fix, nothing to print: %q", buf.String())
	}
}

func TestPsqlConnectQuotesTheName(t *testing.T) {
	cases := map[string]string{"crm": `\connect "crm"`, "Sales-2024": `\connect "Sales-2024"`, `a"b`: `\connect "a""b"`}
	for name, want := range cases {
		if got := psqlConnect(name); got != want {
			t.Errorf("psqlConnect(%q) = %s, want %s", name, got, want)
		}
	}
}
