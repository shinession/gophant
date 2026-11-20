//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The multi-database scenario: s1 has seven databases and gophant finds them itself.
//
//	appdb       the seeded database of the other tests; its publication exists, s3 has the database
//	Sales-2024  a name that needs quoting everywhere; s3 does not have it, s1 has no publication for it
//	crmdb       owned by a role; s3 does not have the database either
//	broken      a table without a primary key, so its precheck fails and it stays out of the run
//	locked      the source role may not connect until it becomes a superuser; it has no table
//	emptydb, postgres  no table: skipped
//
// "gophant run" copies the first three and streams them while a writer changes each of them; it is
// killed and started again; the cutover, the verification and the release of the slots follow.

const multiConfigInPod = "/e2e/gophant.e2e.multi.yaml"

const salesSchema = `
CREATE TABLE orders (id bigserial PRIMARY KEY, customer text NOT NULL, amount numeric(10,2), placed timestamptz NOT NULL DEFAULT now());
CREATE TABLE audit (ts timestamptz NOT NULL DEFAULT now(), msg text);
ALTER TABLE audit REPLICA IDENTITY FULL;
INSERT INTO orders (customer, amount) SELECT 'c' || g, g / 10.0 FROM generate_series(1, 5000) g;
INSERT INTO audit (msg) SELECT 'm' || g FROM generate_series(1, 500) g;
`

const crmSchema = `
CREATE TABLE contacts (id serial PRIMARY KEY, name text NOT NULL, email text UNIQUE);
INSERT INTO contacts (name, email) SELECT 'n' || g, 'seed' || g || '@example.com' FROM generate_series(1, 2000) g;
ALTER TABLE contacts OWNER TO crm_owner;
`

// gophantMulti runs the gophant binary in the s3 container with the multi-database configuration.
func gophantMulti(t *testing.T, lagTimeout string, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"compose", "-f", composeFile, "exec", "-T",
		"-e", "GOPHANT_E2E_PASSWORD=" + password(), "-e", "GOPHANT_E2E_LAG_TIMEOUT=" + lagTimeout,
		"s3", "/gophant/" + binaryName}, args...)
	full = append(full, "-c", multiConfigInPod, "--log-format", "text")
	out, err := run(t, "docker", full...)
	t.Logf("gophant %s:\n%s", strings.Join(args, " "), tail(out, 6000))
	return out, err
}

func mustGophantMulti(t *testing.T, args ...string) string {
	t.Helper()
	out, err := gophantMulti(t, "2m", args...)
	if err != nil {
		t.Fatalf("gophant %s failed: %v", strings.Join(args, " "), err)
	}
	return out
}

func connectDatabase(t *testing.T, port, db string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := pgx.ParseConfig(fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%s/postgres?sslmode=disable", password(), port))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = db
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to database %s on port %s: %v", db, port, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// ---- a gophant command that keeps running inside the s3 container -----------------------------

// startBackground starts gophant with args in the background of the s3 container. Its output goes to
// /tmp/<name>.log, its pid to /tmp/<name>.pid and, when it ends, its exit code to /tmp/<name>.exit.
// The shell stays around to record the exit code; $! is the pid of gophant itself.
func startBackground(t *testing.T, name string, args ...string) {
	t.Helper()
	script := `rm -f /tmp/` + name + `.exit; /gophant/` + binaryName + ` ` + strings.Join(args, " ") + ` -c ` + multiConfigInPod +
		` --log-format text >> /tmp/` + name + `.log 2>&1 & echo $! > /tmp/` + name + `.pid; wait $!; echo $? > /tmp/` + name + `.exit`
	compose(t, "exec", "-d", "-e", "GOPHANT_E2E_PASSWORD="+password(), "-e", "GOPHANT_E2E_LAG_TIMEOUT=2m", "s3", "sh", "-c", script)
}

func backgroundAlive(t *testing.T, name string) bool {
	t.Helper()
	out, _ := run(t, "docker", "compose", "-f", composeFile, "exec", "-T", "s3", "sh", "-c",
		"test -f /tmp/"+name+".pid && kill -0 $(cat /tmp/"+name+".pid) 2>/dev/null && echo alive || echo dead")
	return strings.Contains(out, "alive")
}

func signalBackground(t *testing.T, name, signal string) {
	t.Helper()
	compose(t, "exec", "-T", "s3", "sh", "-c", "kill -"+signal+" $(cat /tmp/"+name+".pid)")
}

func backgroundLog(t *testing.T, name string) string {
	t.Helper()
	return compose(t, "exec", "-T", "s3", "sh", "-c", "cat /tmp/"+name+".log")
}

func clearBackgroundLog(t *testing.T, name string) {
	t.Helper()
	compose(t, "exec", "-T", "s3", "sh", "-c", ": > /tmp/"+name+".log")
}

func backgroundExitCode(t *testing.T, name string) string {
	t.Helper()
	return strings.TrimSpace(compose(t, "exec", "-T", "s3", "sh", "-c", "cat /tmp/"+name+".exit"))
}

func startRun(t *testing.T)           { startBackground(t, "run", "run", "--yes") }
func runAlive(t *testing.T) bool      { return backgroundAlive(t, "run") }
func killRun(t *testing.T)            { signalBackground(t, "run", "KILL") }
func runLog(t *testing.T) string      { return backgroundLog(t, "run") }
func runExitCode(t *testing.T) string { return backgroundExitCode(t, "run") }

// ---- what gophant status says ------------------------------------------------------------------

type multiStatus struct {
	Databases []struct {
		Database string `json:"database"`
		Status   *struct {
			Stage string `json:"stage"`
			Slot  *struct {
				Name   string `json:"name"`
				Active bool   `json:"active"`
			} `json:"slot"`
		} `json:"status"`
	} `json:"databases"`
}

// statuses returns the stage of every database that has started, and whether its slot is active.
func statuses(t *testing.T) (stage map[string]string, active map[string]bool) {
	t.Helper()
	out, err := gophantMulti(t, "2m", "status", "--json", "--log-level", "error")
	if err != nil {
		t.Fatalf("status failed: %v", err)
	}
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("no JSON in the status output:\n%s", out)
	}
	var s multiStatus
	if err := json.Unmarshal([]byte(out[start:]), &s); err != nil {
		t.Fatalf("status --json is not valid JSON: %v\n%s", err, out)
	}
	stage, active = map[string]string{}, map[string]bool{}
	for _, d := range s.Databases {
		if d.Status != nil {
			stage[d.Database] = d.Status.Stage
			active[d.Database] = d.Status.Slot != nil && d.Status.Slot.Active
		}
	}
	return stage, active
}

// ---- writers -----------------------------------------------------------------------------------

// writer changes one database of s1 with small statements until it is stopped.
type writer struct {
	committed atomic.Int64
	stop      chan struct{}
	done      chan struct{}
}

func startWriter(t *testing.T, db string, step func(n int) (string, []any)) *writer {
	t.Helper()
	conn := connectDatabase(t, s1Port, db)
	w := &writer{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for n := 1; ; n++ {
			select {
			case <-w.stop:
				return
			default:
			}
			sql, args := step(n)
			if _, err := conn.Exec(context.Background(), sql, args...); err == nil {
				w.committed.Add(1)
			}
			time.Sleep(3 * time.Millisecond)
		}
	}()
	return w
}

func (w *writer) Stop() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.done
}

func salesStep(n int) (string, []any) {
	switch n % 5 {
	case 0:
		return `INSERT INTO orders (customer, amount) VALUES ($1, $2)`, []any{fmt.Sprintf("late%d", n), float64(n%1000) / 10}
	case 1:
		return `UPDATE orders SET amount = amount + 1 WHERE id = (SELECT max(id) FROM orders)`, nil
	case 2:
		return `INSERT INTO audit (msg) VALUES ($1)`, []any{fmt.Sprintf("late%d", n)}
	case 3:
		return `UPDATE audit SET msg = msg || '!' WHERE ctid = (SELECT ctid FROM audit ORDER BY random() LIMIT 1)`, nil
	}
	return `DELETE FROM audit WHERE ctid = (SELECT ctid FROM audit ORDER BY random() LIMIT 1)`, nil
}

func crmStep(n int) (string, []any) {
	if n%3 == 0 {
		return `UPDATE contacts SET name = name || 'x' WHERE id = 1 + (random() * 1999)::int`, nil
	}
	return `INSERT INTO contacts (name, email) VALUES ($1, $2)`, []any{fmt.Sprintf("late%d", n), fmt.Sprintf("late%d@example.com", n)}
}

// tablesFingerprint returns the row count and a hash of the rows of every table.
func tablesFingerprint(t *testing.T, conn *pgx.Conn, tables ...string) string {
	t.Helper()
	var parts []string
	for _, table := range tables {
		parts = append(parts, table+"="+queryString(t, conn,
			fmt.Sprintf(`SELECT count(*) || ':' || coalesce(md5(string_agg(x::text, E'\n' ORDER BY x::text)), '') FROM %s x`, table)))
	}
	return strings.Join(parts, "\n")
}

func TestMultiDatabase(t *testing.T) {
	s1, s3 := startEnvironment(t) // appdb is seeded and has its publication

	t.Log("== more databases on s1")
	exec1(t, s1, `CREATE DATABASE "Sales-2024"`)
	exec1(t, s1, `CREATE ROLE crm_owner`)
	exec1(t, s1, `CREATE DATABASE crmdb OWNER crm_owner`)
	exec1(t, s1, `CREATE DATABASE emptydb`)
	exec1(t, s1, `CREATE DATABASE broken`)
	exec1(t, s1, `CREATE DATABASE locked`)
	exec1(t, s1, `REVOKE CONNECT ON DATABASE locked FROM PUBLIC`)
	sales := connectDatabase(t, s1Port, "Sales-2024")
	exec1(t, sales, salesSchema)
	crm := connectDatabase(t, s1Port, "crmdb")
	exec1(t, crm, crmSchema)
	exec1(t, connectDatabase(t, s1Port, "broken"), `CREATE TABLE events (id int, payload text); INSERT INTO events VALUES (1, 'no key')`)

	t.Log("== precheck of a cluster that is not ready: the role is missing on s3, the source role is no superuser")
	out, err := gophantMulti(t, "2m", "precheck")
	if err == nil {
		t.Fatalf("the precheck must fail:\n%s", out)
	}
	for _, want := range []string{
		"gophant precheck report (7 database(s))",
		"[FAIL] target.roles", "crm_owner",
		"may not connect to database locked",
		"[FAIL] source.publication",
		`\connect "Sales-2024"`, `CREATE PUBLICATION "gophant_pub" FOR ALL TABLES`,
		"-- broken: fix source.replica_identity first",
		"skipped: no table in scope",
		"will be created",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the first precheck misses %q", want)
		}
	}

	t.Log("== fix what the report asked for, except the table of the database 'broken'")
	exec1(t, s3, `CREATE ROLE crm_owner`)
	exec1(t, s1, `ALTER ROLE gophant_repl SUPERUSER`)
	out, err = gophantMulti(t, "2m", "precheck")
	if err == nil {
		t.Fatalf("the precheck must still fail for the database 'broken':\n%s", out)
	}
	for _, want := range []string{"BLOCKED, 1 failure(s)", "Summary: 7 database(s): 3 ready, 1 blocked, 3 skipped"} {
		if !strings.Contains(out, want) {
			t.Errorf("the second precheck misses %q", want)
		}
	}
	if strings.Contains(out, "may not connect") || strings.Contains(out, "[FAIL] target.roles") || strings.Contains(out, "[FAIL] source.publication") {
		t.Errorf("the fixed problems must be gone:\n%s", out)
	}

	t.Log("== gophant run: three databases are copied and streamed, 'broken' is skipped")
	load := startWorkload(t)
	salesWriter := startWriter(t, "Sales-2024", salesStep)
	crmWriter := startWriter(t, "crmdb", crmStep)
	defer func() {
		select {
		case <-load.stop: // stopped by the test already
		default:
			load.Stop()
		}
		salesWriter.Stop()
		crmWriter.Stop()
	}()
	waitFor(t, "the workloads to start", 30*time.Second, func() bool {
		return load.committed.Load() >= 20 && salesWriter.committed.Load() >= 20 && crmWriter.committed.Load() >= 20
	})
	startRun(t)
	waitFor(t, "all three databases to stream", 4*time.Minute, func() bool {
		stage, active := statuses(t)
		for _, db := range []string{"appdb", "Sales-2024", "crmdb"} {
			if stage[db] != "CDC_STREAM" || !active[db] {
				return false
			}
		}
		return true
	})

	t.Log("== what run created")
	for _, db := range []string{"Sales-2024", "crmdb"} {
		if n := queryString(t, s3, `SELECT count(*)::text FROM pg_database WHERE datname = $1`, db); n != "1" {
			t.Errorf("s3 does not have the database %s", db)
		}
	}
	if n := queryString(t, s3, `SELECT count(*)::text FROM pg_database WHERE datname IN ('broken', 'emptydb', 'locked')`); n != "0" {
		t.Errorf("s3 must not get the databases that were skipped or blocked, found %s of them", n)
	}
	const props = `SELECT string_agg(datname || ' ' || pg_encoding_to_char(encoding) || ' ' || datcollate || ' ' || datctype, E'\n' ORDER BY datname)
FROM pg_database WHERE datname IN ('appdb', 'Sales-2024', 'crmdb')`
	if a, b := queryString(t, s1, props), queryString(t, s3, props); a != b {
		t.Errorf("encoding and collation differ:\n--- s1\n%s\n--- s3\n%s", a, b)
	}
	if owner := queryString(t, s3, `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'crmdb'`); owner != "crm_owner" {
		t.Errorf("the owner of crmdb on s3 is %s, want crm_owner", owner)
	}
	for name, conn := range map[string]*pgx.Conn{"Sales-2024": sales, "crmdb": crm} {
		if n := queryString(t, conn, `SELECT count(*)::text FROM pg_publication WHERE pubname = 'gophant_pub' AND puballtables AND pubviaroot`); n != "1" {
			t.Errorf("the publication was not created in %s", name)
		}
	}
	slotPattern := regexp.MustCompile(`^gophant_slot_(appdb|crmdb|sales_2024_[0-9a-f]{8})$`)
	var slots []string
	rows, err := s1.Query(context.Background(), `SELECT slot_name FROM pg_replication_slots WHERE active ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		slots = append(slots, name)
	}
	rows.Close()
	if len(slots) != 3 {
		t.Fatalf("expected three active slots, got %v", slots)
	}
	for _, s := range slots {
		if !slotPattern.MatchString(s) {
			t.Errorf("unexpected slot %q", s)
		}
	}

	t.Log("== snapshot leaves a complete database alone")
	out = mustGophantMulti(t, "snapshot", "-d", "appdb")
	if !strings.Contains(out, "skipped: the snapshot is already complete") {
		t.Errorf("a complete snapshot must be skipped:\n%s", out)
	}

	t.Log("== status")
	out = mustGophantMulti(t, "status")
	for _, want := range []string{"gophant status (7 database(s))", "CDC_STREAM", "not started"} {
		if !strings.Contains(out, want) {
			t.Errorf("status misses %q:\n%s", want, out)
		}
	}

	t.Log("== run is killed and started again: nothing is copied twice")
	time.Sleep(3 * time.Second)
	killRun(t)
	waitFor(t, "the killed run to be gone", 20*time.Second, func() bool { return !runAlive(t) })
	time.Sleep(3 * time.Second) // the writers go on while nothing replays
	clearBackgroundLog(t, "run")
	startRun(t)
	waitFor(t, "the restarted run to stream every database", 90*time.Second, func() bool {
		_, active := statuses(t)
		return active["appdb"] && active["Sales-2024"] && active["crmdb"]
	})
	second := runLog(t)
	for _, unwanted := range []string{"copying table", "creating the database on s3", "replication slot created"} {
		if strings.Contains(second, unwanted) {
			t.Errorf("the second run must not do %q again:\n%s", unwanted, second)
		}
	}
	time.Sleep(4 * time.Second)

	t.Log("== stop the applications")
	load.Stop()
	salesWriter.Stop()
	crmWriter.Stop()
	t.Logf("committed on s1 while gophant ran: appdb %d, Sales-2024 %d, crmdb %d", load.committed.Load(), salesWriter.committed.Load(), crmWriter.committed.Load())

	t.Log("== a cutover that would leave a database behind is refused; the databases without a table are not left behind")
	out, err = gophantMulti(t, "2m", "cutover", "--yes")
	if err == nil || !strings.Contains(out, "of 1 database(s) is not finished (broken)") {
		t.Errorf("the cutover must refuse because of 'broken' alone (err=%v):\n%s", err, out)
	}
	if strings.Contains(out, "emptydb") || strings.Contains(out, "(postgres") {
		t.Errorf("a database without a table must not stop the cutover:\n%s", out)
	}

	t.Log("== cutover of the three databases")
	out = mustGophantMulti(t, "cutover", "--yes", "-d", "appdb", "-d", "Sales-2024", "-d", "crmdb")
	if n := strings.Count(out, "Cutover complete"); n != 3 {
		t.Errorf("expected three completed cutovers, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "Summary of cutover: 3 database(s), 3 ok, 0 failed") {
		t.Errorf("the cutover summary is wrong:\n%s", out)
	}
	waitFor(t, "run to end by itself after the cutover", 60*time.Second, func() bool { return !runAlive(t) })
	if code := runExitCode(t); code == "0" {
		t.Errorf("run must exit with an error because 'broken' was skipped, exit code %s", code)
	}
	log := runLog(t)
	for _, want := range []string{"broken", "FAILED: blocked by the precheck", "Summary of run: 7 database(s), 6 ok, 1 failed"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log of run misses %q:\n%s", want, tail(log, 3000))
		}
	}

	t.Log("== the databases are identical")
	sales3, crm3 := connectDatabase(t, s3Port, "Sales-2024"), connectDatabase(t, s3Port, "crmdb")
	if a, b := fingerprints(t, s1), fingerprints(t, s3); a != b {
		t.Errorf("appdb differs:\n--- s1\n%s\n--- s3\n%s", a, b)
	}
	if a, b := tablesFingerprint(t, sales, "orders", "audit"), tablesFingerprint(t, sales3, "orders", "audit"); a != b {
		t.Errorf("Sales-2024 differs:\n--- s1\n%s\n--- s3\n%s", a, b)
	}
	if a, b := tablesFingerprint(t, crm, "contacts"), tablesFingerprint(t, crm3, "contacts"); a != b {
		t.Errorf("crmdb differs:\n--- s1\n%s\n--- s3\n%s", a, b)
	}
	last := queryString(t, sales, `SELECT last_value::text FROM pg_sequences WHERE sequencename = 'orders_id_seq'`)
	got := queryString(t, sales3, `SELECT last_value::text FROM pg_sequences WHERE sequencename = 'orders_id_seq'`)
	if l, _ := strconv.ParseInt(last, 10, 64); got != strconv.FormatInt(l+100, 10) {
		t.Errorf("sequence of Sales-2024: s1 %s, s3 %s, want s1 + 100", last, got)
	}

	t.Log("== verify, then release the slots")
	out = mustGophantMulti(t, "verify", "-d", "appdb", "-d", "Sales-2024", "-d", "crmdb")
	if !strings.Contains(out, "Summary of verify: 3 database(s), 3 ok, 0 failed") || !strings.Contains(out, "gophant verify --drop-slot") {
		t.Errorf("verify did not pass:\n%s", out)
	}
	if n := queryString(t, s1, `SELECT count(*)::text FROM pg_replication_slots WHERE slot_name LIKE 'gophant_slot%'`); n != "3" {
		t.Fatalf("the three slots must exist until --drop-slot, count = %s", n)
	}
	mustGophantMulti(t, "verify", "--drop-slot", "-d", "appdb", "-d", "Sales-2024", "-d", "crmdb")
	if n := queryString(t, s1, `SELECT count(*)::text FROM pg_replication_slots WHERE slot_name LIKE 'gophant_slot%'`); n != "0" {
		t.Errorf("the slots must be gone after --drop-slot, count = %s", n)
	}
	for name, conn := range map[string]*pgx.Conn{"appdb": s3, "Sales-2024": sales3, "crmdb": crm3} {
		if stage := queryString(t, conn, `SELECT stage FROM gophant_meta.sync_state`); stage != "DONE" {
			t.Errorf("final stage of %s = %s, want DONE", name, stage)
		}
	}

	t.Log("== without --database, the databases without a table are skipped and only 'broken' fails")
	out, err = gophantMulti(t, "2m", "verify")
	if err == nil {
		t.Fatalf("verify must fail for 'broken', which was never migrated:\n%s", out)
	}
	for _, want := range []string{"Summary of verify: 7 database(s), 6 ok, 1 failed", "FAILED: the migration of this database has not started"} {
		if !strings.Contains(out, want) {
			t.Errorf("verify misses %q", want)
		}
	}
	if n := strings.Count(out, "skipped: no table in scope, nothing to copy"); n != 3 {
		t.Errorf("expected three databases without a table, got %d", n)
	}
	out = mustGophantMulti(t, "status")
	for _, want := range []string{"DONE", "not started", "skipped: no table in scope"} {
		if !strings.Contains(out, want) {
			t.Errorf("the final status misses %q:\n%s", want, out)
		}
	}
}

// TestMultiDatabaseStages uses the stages one by one instead of "gophant run": snapshot of two
// databases, sync of both in one process, a restart of one snapshot, cutover and verification.
func TestMultiDatabaseStages(t *testing.T) {
	s1, _ := startEnvironment(t)
	dbs := []string{"-d", "Sales-2024", "-d", "crmdb"}
	with := func(args ...string) []string { return append(args, dbs...) }

	exec1(t, s1, `ALTER ROLE gophant_repl SUPERUSER`) // gophant creates the publications
	exec1(t, s1, `CREATE DATABASE "Sales-2024"`)
	exec1(t, s1, `CREATE DATABASE crmdb`)
	sales := connectDatabase(t, s1Port, "Sales-2024")
	exec1(t, sales, salesSchema)
	crm := connectDatabase(t, s1Port, "crmdb")
	exec1(t, crm, strings.Replace(crmSchema, "ALTER TABLE contacts OWNER TO crm_owner;", "", 1))

	t.Log("== snapshot of two databases")
	out := mustGophantMulti(t, with("snapshot")...)
	for _, want := range []string{"[Sales-2024] Snapshot complete.", "[crmdb] Snapshot complete.", "Summary of snapshot: 2 database(s), 2 ok, 0 failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("snapshot misses %q", want)
		}
	}
	sales3, crm3 := connectDatabase(t, s3Port, "Sales-2024"), connectDatabase(t, s3Port, "crmdb")
	for name, conn := range map[string]*pgx.Conn{"Sales-2024": sales3, "crmdb": crm3} {
		if stage := queryString(t, conn, `SELECT stage FROM gophant_meta.sync_state`); stage != "CDC_STREAM" {
			t.Errorf("stage of %s after the snapshot = %s", name, stage)
		}
	}
	if n := queryString(t, s1, `SELECT count(*)::text FROM pg_replication_slots WHERE slot_name LIKE 'gophant_slot%'`); n != "2" {
		t.Errorf("expected two slots, got %s", n)
	}
	out = mustGophantMulti(t, with("snapshot")...)
	if n := strings.Count(out, "skipped: the snapshot is already complete"); n != 2 {
		t.Errorf("a second snapshot must skip both databases, got %d skipped:\n%s", n, out)
	}

	t.Log("== sync of both databases in one process")
	startBackground(t, "sync", with("sync")...)
	activeSlots := func() string {
		return queryString(t, s1, `SELECT count(*)::text FROM pg_replication_slots WHERE slot_name LIKE 'gophant_slot%' AND active`)
	}
	waitFor(t, "both streams to run", 60*time.Second, func() bool { return activeSlots() == "2" })
	salesWriter := startWriter(t, "Sales-2024", salesStep)
	crmWriter := startWriter(t, "crmdb", crmStep)
	time.Sleep(5 * time.Second)
	salesWriter.Stop()
	crmWriter.Stop()
	waitFor(t, "s3 to catch up", 60*time.Second, func() bool {
		return tablesFingerprint(t, sales, "orders", "audit") == tablesFingerprint(t, sales3, "orders", "audit") &&
			tablesFingerprint(t, crm, "contacts") == tablesFingerprint(t, crm3, "contacts")
	})

	t.Log("== a restart cannot drop a slot that sync is consuming, and changes nothing")
	out, err := gophantMulti(t, "2m", "snapshot", "--restart", "--yes", "-d", "crmdb")
	if err == nil || !strings.Contains(out, "still in use") {
		t.Fatalf("the restart must fail while sync consumes the slot (err=%v):\n%s", err, out)
	}
	if stage := queryString(t, crm3, `SELECT stage FROM gophant_meta.sync_state`); stage != "CDC_STREAM" {
		t.Errorf("a failed restart must leave the stage alone, got %s", stage)
	}
	if a, b := tablesFingerprint(t, crm, "contacts"), tablesFingerprint(t, crm3, "contacts"); a != b {
		t.Errorf("a failed restart must leave the rows alone:\n--- s1\n%s\n--- s3\n%s", a, b)
	}

	t.Log("== sync is stopped with SIGTERM, then the restart of crmdb works")
	signalBackground(t, "sync", "TERM")
	waitFor(t, "sync to stop", 30*time.Second, func() bool { return !backgroundAlive(t, "sync") })
	if code := backgroundExitCode(t, "sync"); code != "0" {
		t.Errorf("a stopped sync must exit with 0, got %s", code)
	}
	exec1(t, crm, `INSERT INTO contacts (name, email) VALUES ('between the snapshots', 'between@example.com')`)
	out = mustGophantMulti(t, "snapshot", "--restart", "--yes", "-d", "crmdb")
	if !strings.Contains(out, "[crmdb] Snapshot complete.") {
		t.Errorf("the restarted snapshot did not complete:\n%s", out)
	}
	if a, b := tablesFingerprint(t, crm, "contacts"), tablesFingerprint(t, crm3, "contacts"); a != b {
		t.Errorf("crmdb differs after the restart:\n--- s1\n%s\n--- s3\n%s", a, b)
	}

	t.Log("== sync again, cutover, verify")
	clearBackgroundLog(t, "sync")
	startBackground(t, "sync", with("sync")...)
	waitFor(t, "both streams to run again", 60*time.Second, func() bool { return activeSlots() == "2" })
	out = mustGophantMulti(t, with("cutover", "--yes")...)
	if n := strings.Count(out, "Cutover complete"); n != 2 {
		t.Errorf("expected two completed cutovers, got %d:\n%s", n, out)
	}
	waitFor(t, "sync to end by itself", 60*time.Second, func() bool { return !backgroundAlive(t, "sync") })
	if code := backgroundExitCode(t, "sync"); code != "0" {
		t.Errorf("sync must exit with 0 after the cutover, got %s", code)
	}
	if log := backgroundLog(t, "sync"); !strings.Contains(log, "Summary of sync: 2 database(s), 2 ok, 0 failed") {
		t.Errorf("the summary of sync is missing:\n%s", tail(log, 2000))
	}
	if a, b := tablesFingerprint(t, sales, "orders", "audit"), tablesFingerprint(t, sales3, "orders", "audit"); a != b {
		t.Errorf("Sales-2024 differs:\n--- s1\n%s\n--- s3\n%s", a, b)
	}
	out = mustGophantMulti(t, with("verify", "--drop-slot")...)
	if !strings.Contains(out, "Summary of verify: 2 database(s), 2 ok, 0 failed") {
		t.Errorf("verify did not pass:\n%s", out)
	}
	if n := queryString(t, s1, `SELECT count(*)::text FROM pg_replication_slots WHERE slot_name LIKE 'gophant_slot%'`); n != "0" {
		t.Errorf("the slots must be gone, count = %s", n)
	}
}
