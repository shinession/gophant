//go:build e2e

// Package e2e runs the whole migration against real servers: PostgreSQL 16 as s1 and PostgreSQL 18 as
// s3, started with Docker Compose. See docker-compose.yml. Run it with "make e2e".
//
// The scenario: a workload writes to s1 all the time; the snapshot is taken under that load; the WAL
// replay is interrupted twice (a dropped connection, a killed process) and must resume without loss or
// duplicate; the cutover refuses to finish while a write transaction is open and completes when it ends;
// the sequences move; verification and an independent fingerprint prove s3 equals s1.
package e2e

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	composeFile = "docker-compose.yml"
	s1Port      = "55416"
	s3Port      = "55418"
	binaryName  = "gophant-linux-amd64"
	configInPod = "/e2e/gophant.e2e.yaml"
)

var (
	passwordOnce  sync.Once
	passwordValue string
)

// password is the password of the test containers: GOPHANT_E2E_PASSWORD when set, otherwise a random
// one that lives for this test run only.
func password() string {
	passwordOnce.Do(func() {
		if p := os.Getenv("GOPHANT_E2E_PASSWORD"); p != "" {
			passwordValue = p
			return
		}
		b := make([]byte, 12)
		if _, err := cryptorand.Read(b); err != nil {
			panic(err)
		}
		passwordValue = hex.EncodeToString(b)
	})
	return passwordValue
}

// run executes a command in the e2e directory and returns its combined output.
func run(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "GOPHANT_E2E_PASSWORD="+password())
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func compose(t *testing.T, args ...string) string {
	t.Helper()
	out, err := run(t, "docker", append([]string{"compose", "-f", composeFile}, args...)...)
	if err != nil {
		t.Fatalf("docker compose %s failed: %v\n%s", strings.Join(args, " "), err, tail(out, 3000))
	}
	return out
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// gophant runs the gophant binary inside the s3 container.
func gophant(t *testing.T, lagTimeout string, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"compose", "-f", composeFile, "exec", "-T",
		"-e", "GOPHANT_E2E_PASSWORD=" + password(), "-e", "GOPHANT_E2E_LAG_TIMEOUT=" + lagTimeout,
		"s3", "/gophant/" + binaryName}, args...)
	full = append(full, "-c", configInPod, "--log-format", "text")
	out, err := run(t, "docker", full...)
	t.Logf("gophant %s:\n%s", strings.Join(args, " "), tail(out, 4000))
	return out, err
}

func mustGophant(t *testing.T, args ...string) string {
	t.Helper()
	out, err := gophant(t, "2m", args...)
	if err != nil {
		t.Fatalf("gophant %s failed: %v", strings.Join(args, " "), err)
	}
	return out
}

// connect opens a connection to a container. It retries for up to 30 seconds: the health check of the
// image can pass while the server is still in its initialization phase, and the first connection from
// the host then ends with "unexpected EOF".
func connect(t *testing.T, port string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dsn := fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%s/appdb?sslmode=disable", password(), port)
	for {
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			t.Cleanup(func() { _ = conn.Close(context.Background()) })
			return conn
		}
		select {
		case <-ctx.Done():
			t.Fatalf("connect to port %s: %v", port, err)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func exec1(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func queryString(t *testing.T, conn *pgx.Conn, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return s
}

// waitFor polls until cond returns true.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// ---- the sync process inside the s3 container ------------------------------------------------

func startSync(t *testing.T) {
	t.Helper()
	script := `echo $$ > /tmp/sync.pid; exec /gophant/` + binaryName + ` sync -c ` + configInPod + ` --log-format text --log-level debug >> /tmp/sync.log 2>&1`
	compose(t, "exec", "-d", "-e", "GOPHANT_E2E_PASSWORD="+password(), "-e", "GOPHANT_E2E_LAG_TIMEOUT=2m", "s3", "sh", "-c", script)
}

func syncAlive(t *testing.T) bool {
	t.Helper()
	out, _ := run(t, "docker", "compose", "-f", composeFile, "exec", "-T", "s3", "sh", "-c",
		"test -f /tmp/sync.pid && kill -0 $(cat /tmp/sync.pid) 2>/dev/null && echo alive || echo dead")
	return strings.Contains(out, "alive")
}

func killSync(t *testing.T, signal string) {
	t.Helper()
	compose(t, "exec", "-T", "s3", "sh", "-c", "kill -"+signal+" $(cat /tmp/sync.pid)")
}

func syncLog(t *testing.T) string {
	t.Helper()
	return compose(t, "exec", "-T", "s3", "sh", "-c", "cat /tmp/sync.log")
}

// ---- the workload ----------------------------------------------------------------------------

// workload writes to s1 with small random transactions until it is stopped. Failed transactions are
// rolled back and counted; the test only needs the committed ones to reach s3.
type workload struct {
	conn      *pgx.Conn
	committed atomic.Int64
	failed    atomic.Int64
	stop      chan struct{}
	done      chan struct{}
}

func startWorkload(t *testing.T) *workload {
	w := &workload{conn: connect(t, s1Port), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		rng := rand.New(rand.NewPCG(7, 11))
		var n int
		for {
			select {
			case <-w.stop:
				return
			default:
			}
			n++
			if err := w.oneTransaction(rng, n); err != nil {
				w.failed.Add(1)
			} else {
				w.committed.Add(1)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return w
}

func (w *workload) oneTransaction(rng *rand.Rand, n int) error {
	ctx := context.Background()
	tx, err := w.conn.Begin(ctx)
	if err != nil {
		return err
	}
	for i := 1 + rng.IntN(4); i > 0; i-- {
		var sql string
		var args []any
		switch rng.IntN(12) {
		case 0:
			sql, args = `INSERT INTO customers (name, email, profile) VALUES ($1, $2, '{"w":1}')`, []any{fmt.Sprintf("w%d", n), fmt.Sprintf("w%d-%d@example.com", n, i)}
		case 1:
			sql, args = `UPDATE customers SET name = $1 WHERE id = $2`, []any{fmt.Sprintf("u%d", n), 1 + rng.IntN(2000)}
		case 2:
			sql, args = `INSERT INTO orders (customer_id, amount, note, tags) VALUES ($1, $2, $3, $4::mood[])`,
				[]any{1 + rng.IntN(2000), float64(rng.IntN(10000)) / 100, nullable(rng, fmt.Sprintf("n%d", n)), "{ok,happy}"}
		case 3:
			sql, args = `UPDATE orders SET amount = amount + 1, note = NULL WHERE order_id = $1`, []any{1 + rng.IntN(5000)}
		case 4:
			sql, args = `DELETE FROM order_items WHERE order_id = $1 AND line_no = $2`, []any{1 + rng.IntN(5000), 1 + rng.IntN(2)}
		case 5:
			sql = `UPDATE blobs SET meta = meta + 1 WHERE id = ` + strconv.Itoa(1+rng.IntN(20)) // TOAST columns untouched
		case 6:
			sql = `UPDATE blobs SET doc = doc || 'x' WHERE id = ` + strconv.Itoa(1+rng.IntN(20))
		case 7:
			sql, args = `INSERT INTO events_nopk (kind, payload, n) VALUES ($1, $2::json, $3)`, []any{fmt.Sprintf("k%d", rng.IntN(5)), `{"w":1}`, nullable(rng, rng.IntN(5))}
		case 8:
			sql = `UPDATE events_nopk SET n = coalesce(n, 0) + 1 WHERE ctid = (SELECT ctid FROM events_nopk ORDER BY random() LIMIT 1)`
		case 9:
			sql = `DELETE FROM events_nopk WHERE ctid = (SELECT ctid FROM events_nopk ORDER BY random() LIMIT 1)`
		case 10:
			sql, args = `INSERT INTO metrics (bucket, val) VALUES (DATE '2026-01-01' + $1::int, $2)`, []any{rng.IntN(59), rng.Float64() * 100}
		default:
			sql, args = `UPDATE bulk SET payload = md5($1::text), grp = grp + 1 WHERE id = $2`, []any{strconv.Itoa(n), 1 + rng.IntN(300000)}
		}
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
	}
	if rng.IntN(10) == 0 {
		return tx.Rollback(ctx) // a share of rolled back transactions, which must never reach s3
	}
	return tx.Commit(ctx)
}

func nullable(rng *rand.Rand, v any) any {
	if rng.IntN(4) == 0 {
		return nil
	}
	return v
}

func (w *workload) Stop() {
	close(w.stop)
	<-w.done
}

// fingerprints returns one line per table with its row count and a hash of all rows.
func fingerprints(t *testing.T, conn *pgx.Conn) string {
	t.Helper()
	var parts []string
	for _, table := range []string{"blobs", "bulk", "customers", "events_nopk", "metrics", "order_items", "orders"} {
		parts = append(parts, table+"="+queryString(t, conn,
			fmt.Sprintf(`SELECT count(*) || ':' || coalesce(md5(string_agg(x::text, E'\n' ORDER BY x::text)), '') FROM %s x`, table)))
	}
	return strings.Join(parts, "\n")
}

// ---- the scenario ----------------------------------------------------------------------------

// startEnvironment builds the binary, starts fresh PostgreSQL 16 and 18 containers, seeds s1 and
// creates the publication. The containers are removed at the end of the test unless GOPHANT_E2E_KEEP is set.
func startEnvironment(t *testing.T) (s1, s3 *pgx.Conn) {
	t.Helper()
	if testing.Short() {
		t.Skip("the end-to-end tests need Docker")
	}
	buildBinary(t)

	compose(t, "down", "-v", "--remove-orphans")
	compose(t, "up", "-d", "--wait", "--quiet-pull")
	if os.Getenv("GOPHANT_E2E_KEEP") == "" {
		t.Cleanup(func() { _, _ = run(t, "docker", "compose", "-f", composeFile, "down", "-v", "--remove-orphans") })
	}

	s1, s3 = connect(t, s1Port), connect(t, s3Port)
	t.Log("== seed s1 and create the publication")
	seed, err := os.ReadFile(filepath.Join("testdata", "seed.sql"))
	if err != nil {
		t.Fatal(err)
	}
	exec1(t, s1, string(seed))
	exec1(t, s1, `CREATE PUBLICATION gophant_pub FOR ALL TABLES WITH (publish_via_partition_root = true)`)
	return s1, s3
}

func TestMigrationEndToEnd(t *testing.T) {
	s1, s3 := startEnvironment(t)

	t.Log("== precheck")
	out := mustGophant(t, "precheck")
	if !strings.Contains(out, "Result: OK") {
		t.Fatalf("precheck did not pass:\n%s", out)
	}

	t.Log("== snapshot under write load")
	load := startWorkload(t)
	defer func() {
		select {
		case <-load.stop:
		default:
			load.Stop()
		}
	}()
	waitFor(t, "the workload to start", 30*time.Second, func() bool { return load.committed.Load() >= 20 })
	before := load.committed.Load()
	out = mustGophant(t, "snapshot")
	if !strings.Contains(out, "Snapshot complete") {
		t.Fatalf("snapshot did not complete:\n%s", out)
	}
	t.Logf("%d transactions were committed on s1 while the snapshot ran", load.committed.Load()-before)
	if stage := queryString(t, s3, `SELECT stage FROM gophant_meta.sync_state`); stage != "CDC_STREAM" {
		t.Fatalf("stage after the snapshot = %s", stage)
	}

	t.Log("== a second snapshot must be refused")
	if out, err := gophant(t, "2m", "snapshot"); err == nil || !strings.Contains(out, "already complete") {
		t.Fatalf("a finished snapshot must not run again without --restart (err=%v)", err)
	}

	t.Log("== sync, interrupted by a dropped connection")
	startSync(t)
	waitFor(t, "the slot to become active", 30*time.Second, func() bool {
		return queryString(t, s1, `SELECT coalesce((SELECT active::text FROM pg_replication_slots WHERE slot_name = 'gophant_slot'), 'false')`) == "true"
	})
	time.Sleep(4 * time.Second)
	exec1(t, s1, `SELECT pg_terminate_backend(active_pid) FROM pg_replication_slots WHERE slot_name = 'gophant_slot' AND active`)
	waitFor(t, "sync to reconnect on its own", 60*time.Second, func() bool {
		return strings.Contains(syncLog(t), "the stream stopped") &&
			queryString(t, s1, `SELECT coalesce((SELECT active::text FROM pg_replication_slots WHERE slot_name = 'gophant_slot'), 'false')`) == "true"
	})

	t.Log("== sync, interrupted by a killed process")
	time.Sleep(3 * time.Second)
	killSync(t, "KILL")
	waitFor(t, "the killed sync to be gone", 20*time.Second, func() bool { return !syncAlive(t) })
	time.Sleep(3 * time.Second) // the workload keeps writing while nothing replays
	startSync(t)
	waitFor(t, "the restarted sync to be streaming", 60*time.Second, func() bool {
		return queryString(t, s1, `SELECT coalesce((SELECT active::text FROM pg_replication_slots WHERE slot_name = 'gophant_slot'), 'false')`) == "true"
	})
	time.Sleep(4 * time.Second)

	t.Log("== stop the application: no more writes to s1")
	load.Stop()
	t.Logf("the workload committed %d transactions (%d failed and were rolled back)", load.committed.Load(), load.failed.Load())

	t.Log("== cutover must refuse while a write transaction is open")
	open := connect(t, s1Port)
	tx, err := open.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `INSERT INTO customers (name) VALUES ('never committed')`); err != nil {
		t.Fatal(err)
	}
	out, err = gophant(t, "8s", "cutover", "--yes")
	if err == nil || !strings.Contains(out, "open write transaction") {
		t.Fatalf("the cutover must fail while a write transaction is open (err=%v):\n%s", err, out)
	}
	if stage := queryString(t, s3, `SELECT stage FROM gophant_meta.sync_state`); stage != "CUTOVER" {
		t.Fatalf("after a failed cutover the stage must stay CUTOVER, got %s", stage)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}

	t.Log("== cutover")
	out = mustGophant(t, "cutover", "--yes")
	for _, want := range []string{"Lag is zero", "sequence(s) synchronized", "Cutover complete"} {
		if !strings.Contains(out, want) {
			t.Errorf("cutover output misses %q", want)
		}
	}
	if stage := queryString(t, s3, `SELECT stage FROM gophant_meta.sync_state`); stage != "VERIFY" {
		t.Fatalf("stage after the cutover = %s", stage)
	}
	waitFor(t, "sync to stop by itself after the cutover", 30*time.Second, func() bool { return !syncAlive(t) })
	if log := syncLog(t); !strings.Contains(log, "the stream stopped") {
		t.Error("the dropped connection was not logged as such")
	}

	t.Log("== the two databases are identical")
	if a, b := fingerprints(t, s1), fingerprints(t, s3); a != b {
		t.Fatalf("s1 and s3 differ after the cutover:\n--- s1\n%s\n--- s3\n%s", a, b)
	}

	t.Log("== sequences")
	for _, seq := range []string{"customers_id_seq", "orders_order_id_seq", "metrics_id_seq", "bulk_id_seq"} {
		last := queryString(t, s1, `SELECT last_value::text FROM pg_sequences WHERE sequencename = $1`, seq)
		got := queryString(t, s3, `SELECT last_value::text FROM pg_sequences WHERE sequencename = $1`, seq)
		l, _ := strconv.ParseInt(last, 10, 64)
		if want := strconv.FormatInt(l+100, 10); got != want {
			t.Errorf("sequence %s: s1 last_value %s, s3 %s, want %s (offset 100)", seq, last, got, want)
		}
	}
	if id := queryString(t, s3, `INSERT INTO customers (name) VALUES ('first on s3') RETURNING id::text`); id == "" {
		t.Error("s3 cannot generate an identity value after the cutover")
	}
	exec1(t, s3, `DELETE FROM customers WHERE name = 'first on s3'`) // keep s3 equal to s1 for the next checks

	t.Log("== verify")
	out = mustGophant(t, "verify")
	if !strings.Contains(out, "Result: OK") {
		t.Fatalf("verify did not pass:\n%s", out)
	}
	status := mustGophant(t, "status", "--json")
	// The lag itself is not asserted here: after the cutover the sync has stopped while s1 goes on
	// writing records of its own (autovacuum, checkpoints), so the distance is not zero any more. The
	// cutover waited for lag zero, which its output proves above.
	for _, want := range []string{`"stage": "VERIFY"`, `"lag_bytes":`, `"verified_ok": true`} {
		if !strings.Contains(status, want) {
			t.Errorf("status --json misses %s:\n%s", want, status)
		}
	}
	if strings.Contains(status, `"snapshot_status": "FAILED"`) || strings.Contains(status, `"verified_ok": false`) {
		t.Errorf("status shows a failed table:\n%s", status)
	}

	t.Log("== verify detects a difference")
	original := queryString(t, s3, `SELECT payload FROM bulk WHERE id = 1234`)
	exec1(t, s3, `UPDATE bulk SET payload = 'tampered' WHERE id = 1234`)
	if out, err := gophant(t, "2m", "verify"); err == nil || !strings.Contains(out, "Result: FAILED") {
		t.Fatalf("verify must fail when s3 was tampered with (err=%v)", err)
	}
	exec1(t, s3, `UPDATE bulk SET payload = $1 WHERE id = 1234`, original)
	if out := mustGophant(t, "verify"); !strings.Contains(out, "Result: OK") {
		t.Fatalf("verify must pass again after the repair:\n%s", out)
	}

	t.Log("== verify --drop-slot releases the slot and finishes the migration")
	if n := queryString(t, s1, `SELECT count(*)::text FROM pg_replication_slots WHERE slot_name = 'gophant_slot'`); n != "1" {
		t.Fatalf("the slot must still exist before --drop-slot, count = %s", n)
	}
	mustGophant(t, "verify", "--drop-slot")
	if n := queryString(t, s1, `SELECT count(*)::text FROM pg_replication_slots WHERE slot_name = 'gophant_slot'`); n != "0" {
		t.Errorf("the slot must be gone after --drop-slot, count = %s", n)
	}
	if stage := queryString(t, s3, `SELECT stage FROM gophant_meta.sync_state`); stage != "DONE" {
		t.Errorf("final stage = %s, want DONE", stage)
	}
}

// TestSnapshotRecovery covers the ways a snapshot is repeated: a copy that finished while the post-data
// import failed is continued without copying again, and --restart discards a finished snapshot, asks
// before it does, and ends in the same state.
func TestSnapshotRecovery(t *testing.T) {
	s1, s3 := startEnvironment(t)

	t.Log("== a name on s3 collides with an index of s1, so the post-data import fails")
	exec1(t, s3, `CREATE TABLE orders_customer_idx (x int)`)
	out, err := gophant(t, "2m", "snapshot")
	if err == nil || !strings.Contains(out, "post-data import failed") {
		t.Fatalf("the snapshot must fail in the post-data import (err=%v)", err)
	}
	if stage := queryString(t, s3, `SELECT stage FROM gophant_meta.sync_state`); stage != "SNAPSHOT" {
		t.Fatalf("stage after the failure = %s, want SNAPSHOT", stage)
	}
	if n := queryString(t, s3, `SELECT count(*) FILTER (WHERE snapshot_status = 'DONE') || '/' || count(*) FROM gophant_meta.table_progress`); !strings.HasPrefix(n, "8/8") {
		t.Fatalf("every table must be copied before the post-data import, got %s", n)
	}

	t.Log("== fixing the cause and running snapshot again only repeats the post-data import")
	exec1(t, s3, `DROP TABLE orders_customer_idx`)
	out = mustGophant(t, "snapshot")
	if !strings.Contains(out, "repeating the post-data import") {
		t.Errorf("the second run must continue from the finished copy:\n%s", out)
	}
	if strings.Contains(out, "copying table") {
		t.Errorf("no table may be copied again:\n%s", out)
	}
	if stage := queryString(t, s3, `SELECT stage FROM gophant_meta.sync_state`); stage != "CDC_STREAM" {
		t.Fatalf("stage after the repeated post-data import = %s", stage)
	}
	if a, b := fingerprints(t, s1), fingerprints(t, s3); a != b {
		t.Fatalf("s1 and s3 differ after the snapshot:\n--- s1\n%s\n--- s3\n%s", a, b)
	}

	t.Log("== --restart asks for confirmation and refuses without an answer")
	if out, err := gophant(t, "2m", "snapshot", "--restart"); err == nil || !strings.Contains(out, "--yes") {
		t.Fatalf("--restart without a confirmation must fail and mention --yes (err=%v)", err)
	}
	if n := queryString(t, s1, `SELECT count(*)::text FROM pg_replication_slots WHERE slot_name = 'gophant_slot'`); n != "1" {
		t.Fatalf("a refused restart must not drop the slot, count = %s", n)
	}

	t.Log("== --restart --yes starts over")
	exec1(t, s1, `INSERT INTO customers (name, email) VALUES ('after the first snapshot', 'late@example.com')`)
	out = mustGophant(t, "snapshot", "--restart", "--yes")
	if !strings.Contains(out, "Snapshot complete") {
		t.Fatalf("the restarted snapshot did not complete:\n%s", out)
	}
	if stage := queryString(t, s3, `SELECT stage FROM gophant_meta.sync_state`); stage != "CDC_STREAM" {
		t.Fatalf("stage after the restart = %s", stage)
	}
	if a, b := fingerprints(t, s1), fingerprints(t, s3); a != b {
		t.Fatalf("s1 and s3 differ after the restarted snapshot (the row written between the two snapshots is missing?):\n--- s1\n%s\n--- s3\n%s", a, b)
	}
}

func buildBinary(t *testing.T) {
	t.Helper()
	cmd := exec.Command("go", "build", "-trimpath", "-o", filepath.Join("..", "..", "bin", binaryName), filepath.Join("..", "..", "cmd", "gophant"))
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the linux binary: %v\n%s", err, out)
	}
}
