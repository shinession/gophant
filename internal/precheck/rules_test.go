package precheck

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shinession/gophant/internal/catalog"
	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/meta"
)

func wantSeverity(t *testing.T, got Result, want Severity) {
	t.Helper()
	if got.Severity != want {
		t.Errorf("%s: severity = %s, want %s (summary: %s)", got.ID, got.Severity, want, got.Summary)
	}
}

func TestEvalWALLevel(t *testing.T) {
	wantSeverity(t, evalWALLevel("logical"), SeverityPass)
	wantSeverity(t, evalWALLevel("replica"), SeverityFail)
}

func TestEvalSlotKeepSize(t *testing.T) {
	wantSeverity(t, evalSlotKeepSize(30720, false), SeverityPass)
	wantSeverity(t, evalSlotKeepSize(-1, false), SeverityFail)
	wantSeverity(t, evalSlotKeepSize(0, false), SeverityFail)
	// The configuration may explicitly allow an unlimited slot, which downgrades the FAIL to a WARN.
	wantSeverity(t, evalSlotKeepSize(-1, true), SeverityWarn)
	wantSeverity(t, evalSlotKeepSize(30720, true), SeverityPass)
}

func TestEvalReplicaIdentity(t *testing.T) {
	tbl := func(name string, ident byte, hasPK, hasIdx bool) catalog.Table {
		return catalog.Table{Schema: "public", Name: name, Kind: 'r', ReplicaIdent: ident, HasPK: hasPK, HasReplicaIndex: hasIdx}
	}
	cases := []struct {
		name   string
		tables []catalog.Table
		want   Severity
	}{
		{"primary key with default identity", []catalog.Table{tbl("a", 'd', true, false)}, SeverityPass},
		{"no primary key, default identity", []catalog.Table{tbl("a", 'd', false, false)}, SeverityFail},
		{"nothing", []catalog.Table{tbl("a", 'n', true, false)}, SeverityFail},
		{"full without primary key only warns", []catalog.Table{tbl("a", 'f', false, false)}, SeverityWarn},
		{"full with primary key", []catalog.Table{tbl("a", 'f', true, false)}, SeverityPass},
		{"index identity with an index", []catalog.Table{tbl("a", 'i', false, true)}, SeverityPass},
		{"index identity without an index", []catalog.Table{tbl("a", 'i', false, false)}, SeverityFail},
		{"a failure outweighs a warning", []catalog.Table{tbl("a", 'f', false, false), tbl("b", 'd', false, false)}, SeverityFail},
		{"partitions are decided by their root", []catalog.Table{
			{Schema: "public", Name: "leaf", Kind: 'r', IsPartition: true, ReplicaIdent: 'd'},
		}, SeverityPass},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { wantSeverity(t, evalReplicaIdentity(c.tables), c.want) })
	}

	r := evalReplicaIdentity([]catalog.Table{tbl("events", 'd', false, false)})
	if len(r.Items) != 1 || !strings.Contains(r.Items[0], "public.events") {
		t.Errorf("the offending table must be listed, got %v", r.Items)
	}
	if !strings.Contains(r.Hint, "REPLICA IDENTITY FULL") {
		t.Errorf("the hint must mention REPLICA IDENTITY FULL, got %q", r.Hint)
	}
}

func TestEvalUnlogged(t *testing.T) {
	wantSeverity(t, evalUnlogged([]catalog.Table{{Schema: "s", Name: "t", Kind: 'r', Persistence: 'p'}}), SeverityPass)
	wantSeverity(t, evalUnlogged([]catalog.Table{{Schema: "s", Name: "t", Kind: 'r', Persistence: 'u'}}), SeverityWarn)
	wantSeverity(t, evalUnlogged([]catalog.Table{{Schema: "s", Name: "t", Kind: 'p', Persistence: 'u'}}), SeverityFail)
}

func TestEvalGeneratedColumns(t *testing.T) {
	wantSeverity(t, evalGeneratedColumns([]catalog.Table{{Name: "t", Columns: []catalog.Column{{Name: "a"}}}}), SeverityPass)
	r := evalGeneratedColumns([]catalog.Table{{Schema: "s", Name: "t", Columns: []catalog.Column{{Name: "a"}, {Name: "b", Generated: 's'}}}})
	wantSeverity(t, r, SeverityWarn)
	if len(r.Items) != 1 || !strings.Contains(r.Items[0], "s.t.b") {
		t.Errorf("generated column not listed: %v", r.Items)
	}
}

func TestEvalPublication(t *testing.T) {
	ok := publicationInfo{Exists: true, AllTables: true, Insert: true, Update: true, Delete: true, Truncate: true, ViaPartRoot: true}
	wantSeverity(t, evalPublication("gophant_pub", ok), SeverityPass)

	wantSeverity(t, evalPublication("gophant_pub", publicationInfo{}), SeverityFail)

	noRoot := ok
	noRoot.ViaPartRoot = false
	r := evalPublication("gophant_pub", noRoot)
	wantSeverity(t, r, SeverityFail)
	if !strings.Contains(r.Summary, "publish_via_partition_root") {
		t.Errorf("summary must name publish_via_partition_root: %s", r.Summary)
	}

	noTruncate := ok
	noTruncate.Truncate = false
	wantSeverity(t, evalPublication("gophant_pub", noTruncate), SeverityFail)

	missing := ok
	missing.AllTables = false
	missing.MissingTables = []string{"public.orders"}
	r = evalPublication("gophant_pub", missing)
	wantSeverity(t, r, SeverityFail)
	if len(r.Items) != 1 {
		t.Errorf("missing tables must be listed, got %v", r.Items)
	}
}

func TestEvalPublicationMissing(t *testing.T) {
	// Without the multi-database mode a missing publication is a failure with a statement that fixes it.
	r := evalPublication("gophant_pub", publicationInfo{})
	wantSeverity(t, r, SeverityFail)
	if want := `CREATE PUBLICATION "gophant_pub" FOR ALL TABLES WITH (publish_via_partition_root = true);`; r.Fix != want {
		t.Errorf("Fix = %q, want %q", r.Fix, want)
	}

	// When the snapshot stage creates it, the check passes and offers no fix.
	r = evalPublication("gophant_pub", publicationInfo{WillCreate: true})
	wantSeverity(t, r, SeverityPass)
	if r.Fix != "" || !strings.Contains(r.Summary, "creates it") {
		t.Errorf("unexpected result for a publication that will be created: %+v", r)
	}

	// A publication that exists but is wrong is never fixed automatically.
	bad := publicationInfo{Exists: true, AllTables: true, WillCreate: true}
	r = evalPublication("gophant_pub", bad)
	wantSeverity(t, r, SeverityFail)
	if r.Fix != "" {
		t.Errorf("a wrong publication must not carry a fix: %q", r.Fix)
	}
}

func TestEvalSlot(t *testing.T) {
	tracked := &meta.SyncState{SlotName: "gophant_slot", Stage: meta.StageCDCStream}
	cases := []struct {
		name  string
		slot  slotInfo
		state *meta.SyncState
		want  Severity
	}{
		{"no slot, no state", slotInfo{}, nil, SeverityPass},
		{"slot of this migration", slotInfo{Exists: true, Plugin: "pgoutput", WALStatus: "reserved"}, tracked, SeverityPass},
		{"stale slot without state", slotInfo{Exists: true, Plugin: "pgoutput"}, nil, SeverityFail},
		{"stale slot of another migration", slotInfo{Exists: true, Plugin: "pgoutput"}, &meta.SyncState{SlotName: "other"}, SeverityFail},
		{"invalidated slot", slotInfo{Exists: true, Plugin: "pgoutput", WALStatus: "lost"}, tracked, SeverityFail},
		{"wrong plugin", slotInfo{Exists: true, Plugin: "test_decoding", WALStatus: "reserved"}, tracked, SeverityFail},
		{"slot vanished after the snapshot", slotInfo{}, tracked, SeverityFail},
		{"state exists before the slot was created", slotInfo{}, &meta.SyncState{SlotName: "gophant_slot", Stage: meta.StagePrecheck}, SeverityPass},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { wantSeverity(t, evalSlot("gophant_slot", c.slot, c.state), c.want) })
	}
}

func TestEvalExtensions(t *testing.T) {
	var f config.Filter
	f.IncludeSchemas = []string{"public"}
	src := []extInfo{
		{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"},
		{Name: "pg_trgm", Version: "1.6", Schema: "public"},
	}
	available := map[string]bool{"pg_trgm": true}

	// Available but not installed: pre-data creates it because its schema is in scope.
	wantSeverity(t, evalExtensions(src, map[string]string{}, available, &f, false), SeverityPass)
	// Installed with the same version.
	wantSeverity(t, evalExtensions(src, map[string]string{"pg_trgm": "1.6"}, available, &f, false), SeverityPass)
	// Installed with another version only warns.
	wantSeverity(t, evalExtensions(src, map[string]string{"pg_trgm": "1.5"}, available, &f, false), SeverityWarn)
	// Not available at all.
	wantSeverity(t, evalExtensions(src, map[string]string{}, map[string]bool{}, &f, false), SeverityFail)
	// With skip_ddl nothing creates the extension.
	wantSeverity(t, evalExtensions(src, map[string]string{}, available, &f, true), SeverityFail)
	// An extension in a schema outside the filter is not created by pg_dump.
	outside := []extInfo{{Name: "citext", Version: "1.6", Schema: "ext"}}
	wantSeverity(t, evalExtensions(outside, map[string]string{}, map[string]bool{"citext": true}, &f, false), SeverityFail)
}

func TestEvalRoles(t *testing.T) {
	wantSeverity(t, evalRoles([]string{"app", "ro"}, map[string]bool{"app": true, "ro": true}), SeverityPass)
	r := evalRoles([]string{"app", "ro"}, map[string]bool{"app": true})
	wantSeverity(t, r, SeverityFail)
	if len(r.Items) != 1 || r.Items[0] != "ro" {
		t.Errorf("missing role not listed: %v", r.Items)
	}
}

func TestEvalTargetState(t *testing.T) {
	tables := []catalog.Table{{Schema: "public", Name: "a"}, {Schema: "public", Name: "b"}}

	// Fresh target: nothing may exist when the schema is imported by gophant.
	wantSeverity(t, evalTargetState(tables, map[string]bool{}, false, nil, nil), SeverityPass)
	wantSeverity(t, evalTargetState(tables, map[string]bool{"public.a": true}, false, nil, nil), SeverityFail)

	// skip_ddl: everything must exist already, and be empty.
	both := map[string]bool{"public.a": true, "public.b": true}
	wantSeverity(t, evalTargetState(tables, both, true, nil, nil), SeverityPass)
	wantSeverity(t, evalTargetState(tables, map[string]bool{"public.a": true}, true, nil, nil), SeverityFail)

	// Rows restored from a backup would be duplicated by the copy: this must be a FAIL that names the tables.
	r := evalTargetState(tables, both, true, nil, []string{"public.a"})
	wantSeverity(t, r, SeverityFail)
	if len(r.Items) != 1 || r.Items[0] != "public.a" || !strings.Contains(r.Summary, "contain rows") {
		t.Errorf("the tables with rows must be named: %+v", r)
	}

	// A migration in progress owns the objects of the target, rows included.
	running := &meta.SyncState{Stage: meta.StageCDCStream}
	wantSeverity(t, evalTargetState(tables, both, false, running, nil), SeverityPass)
	wantSeverity(t, evalTargetState(tables, both, true, running, []string{"public.a"}), SeverityPass)
}

func TestEvalTools(t *testing.T) {
	wantSeverity(t, evalTool("pg_dump", 18, 16, nil), SeverityPass)
	wantSeverity(t, evalTool("pg_dump", 17, 16, nil), SeverityWarn)
	wantSeverity(t, evalTool("pg_dump", 15, 16, nil), SeverityFail)
	wantSeverity(t, evalTool("pg_dump", 0, 16, errors.New("not found")), SeverityFail)
}

func TestParseToolMajor(t *testing.T) {
	cases := map[string]int{
		"pg_dump (PostgreSQL) 18.0\n":                  18,
		"pg_restore (PostgreSQL) 17.2 (Debian 17.2-1)": 17,
		"pg_dump (PostgreSQL) 16.4":                    16,
	}
	for in, want := range cases {
		got, err := parseToolMajor(in)
		if err != nil || got != want {
			t.Errorf("parseToolMajor(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := parseToolMajor("something else"); err == nil {
		t.Error("an unexpected output must be an error")
	}
}

func TestEvalVersions(t *testing.T) {
	r := evalVersions(160004, 180000)
	wantSeverity(t, r[0], SeverityPass)
	wantSeverity(t, r[1], SeverityPass)
	r = evalVersions(120000, 180000)
	wantSeverity(t, r[0], SeverityFail)
	r = evalVersions(160000, 150000)
	wantSeverity(t, r[1], SeverityFail)
	r = evalVersions(160000, 170000)
	wantSeverity(t, r[1], SeverityWarn)
}

func TestEvalPrivileges(t *testing.T) {
	all := evalPrivileges(false, true, nil, nil)
	for _, r := range all {
		wantSeverity(t, r, SeverityPass)
	}
	r := evalPrivileges(false, false, []string{"public.secret"}, []string{"public.secret_id_seq"})
	for _, res := range r {
		wantSeverity(t, res, SeverityFail)
	}
	if got := evalPrivileges(true, false, nil, nil); got[0].Severity != SeverityPass {
		t.Error("a superuser needs no REPLICATION attribute")
	}
}

func TestEvalCapacity(t *testing.T) {
	// One database: a slot and a walsender, or no slot when the database has its own already.
	wantSeverity(t, evalCapacity(8, 2, 8, 1, 1, 1), SeverityPass)
	wantSeverity(t, evalCapacity(8, 8, 8, 1, 1, 1), SeverityFail)
	wantSeverity(t, evalCapacity(8, 8, 8, 1, 0, 1), SeverityPass) // the full slots include ours
	wantSeverity(t, evalCapacity(8, 1, 8, 8, 1, 1), SeverityFail)

	// The same text as before for one database.
	if r := evalCapacity(8, 2, 8, 1, 1, 1); strings.Contains(r.Summary, "needs") {
		t.Errorf("a single database must not mention what the run needs: %s", r.Summary)
	}

	// Several databases need a slot and a walsender each.
	wantSeverity(t, evalCapacity(8, 2, 8, 0, 6, 6), SeverityPass)
	r := evalCapacity(8, 3, 8, 0, 6, 6)
	wantSeverity(t, r, SeverityFail)
	if !strings.Contains(r.Summary, "needs 6 more slot(s) and 6 more walsender(s)") {
		t.Errorf("the failure must say what the run needs: %s", r.Summary)
	}
	wantSeverity(t, evalCapacity(8, 0, 8, 3, 5, 6), SeverityFail) // enough slots, too few walsenders
}

func TestEvalScope(t *testing.T) {
	wantSeverity(t, evalScope(nil, nil), SeverityFail)
	tables := []catalog.Table{{Schema: "public", Name: "a", Kind: 'r', TotalBytes: 2048}}
	wantSeverity(t, evalScope(tables, nil), SeverityPass)
	wantSeverity(t, evalScope(tables, []string{"typo"}), SeverityWarn)
}

func TestEvalMonitoring(t *testing.T) {
	wantSeverity(t, evalMonitoring(true), SeverityPass)
	wantSeverity(t, evalMonitoring(false), SeverityWarn)
}

func TestEvalMiscWarnings(t *testing.T) {
	wantSeverity(t, evalMD5Roles(nil, false), SeverityPass)
	wantSeverity(t, evalMD5Roles([]string{"old"}, false), SeverityWarn)
	wantSeverity(t, evalMD5Roles(nil, true), SeverityWarn)
	wantSeverity(t, evalLargeObjects(0), SeverityPass)
	wantSeverity(t, evalLargeObjects(3), SeverityWarn)
	wantSeverity(t, evalBinaryCopy(nil), SeverityPass)
	wantSeverity(t, evalBinaryCopy([]catalog.FormatFallback{{Table: "public.t", Column: "c", Type: "mood[]"}}), SeverityWarn)
	wantSeverity(t, evalLongTransactions(0, 0), SeverityPass)
	wantSeverity(t, evalLongTransactions(2, time.Hour), SeverityWarn)

	same := dbEncoding{Encoding: "UTF8", Collate: "en_US.utf8", Ctype: "en_US.utf8"}
	wantSeverity(t, evalEncoding(same, same), SeverityPass)
	other := same
	other.Collate = "C"
	wantSeverity(t, evalEncoding(same, other), SeverityWarn)
}

func TestReportCountsAndPrint(t *testing.T) {
	var r Report
	r.Add(
		pass("a.ok", "fine"),
		warn("b.warn", "do something", "careful"),
		Result{ID: "c.fail", Severity: SeverityFail, Summary: "broken", Items: []string{"one", "two"}, Hint: "fix it"},
	)
	pass_, warn_, fail_ := r.Counts()
	if pass_ != 1 || warn_ != 1 || fail_ != 1 || !r.Failed() {
		t.Errorf("counts = %d %d %d failed=%v", pass_, warn_, fail_, r.Failed())
	}

	var buf bytes.Buffer
	r.Print(&buf)
	out := buf.String()
	for _, want := range []string{"[PASS] a.ok", "[WARN] b.warn", "[FAIL] c.fail", "- one", "hint: fix it", "BLOCKED"} {
		if !strings.Contains(out, want) {
			t.Errorf("report misses %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hint: do something") == false {
		t.Errorf("a WARN hint must be printed:\n%s", out)
	}
}

func TestPrintResultsCanLeaveOutThePassingChecks(t *testing.T) {
	results := []Result{
		pass("a.ok", "fine"),
		warn("b.warn", "do something", "careful"),
		fail("c.fail", "fix it", "broken"),
	}
	var all, problems bytes.Buffer
	PrintResults(&all, results, false)
	PrintResults(&problems, results, true)
	if !strings.Contains(all.String(), "[PASS] a.ok") {
		t.Errorf("every result is expected:\n%s", all.String())
	}
	if strings.Contains(problems.String(), "a.ok") {
		t.Errorf("a passing check must be left out:\n%s", problems.String())
	}
	for _, want := range []string{"[WARN] b.warn", "[FAIL] c.fail", "hint: fix it"} {
		if !strings.Contains(problems.String(), want) {
			t.Errorf("problems must stay, missing %q:\n%s", want, problems.String())
		}
	}
}

func TestReportPrintCapsLongItemLists(t *testing.T) {
	items := make([]string, 40)
	for i := range items {
		items[i] = "table"
	}
	var r Report
	r.Add(Result{ID: "x", Severity: SeverityWarn, Summary: "many", Items: items})
	var buf bytes.Buffer
	r.Print(&buf)
	if !strings.Contains(buf.String(), "... and 25 more") {
		t.Errorf("long lists must be capped:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "BLOCKED") {
		t.Error("warnings alone must not block")
	}
}
