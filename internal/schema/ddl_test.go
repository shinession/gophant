package schema

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/shinession/gophant/internal/config"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`
source:
  dsn: "postgres://repl:srcsecret@s1:5432/appdb?sslmode=prefer"
target:
  dsn: "host=s3 port=5432 user=postgres password=dstsecret dbname=appdb"
tools:
  pg_dump_bin: /opt/pg18/pg_dump
  pg_restore_bin: /opt/pg18/pg_restore
filter:
  include_schemas: ["public", "sales"]
  exclude_tables: ["public.audit_*"]
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

func newTestRunner(t *testing.T) *Runner {
	return NewRunner(testConfig(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestDumpArgs(t *testing.T) {
	args, env, err := newTestRunner(t).dumpArgs("/tmp/x/schema.dump")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--schema-only", "--format=custom", "--no-publications", "--no-subscriptions", "--no-tablespaces",
		"--file=/tmp/x/schema.dump", "--schema=public", "--schema=sales", "--exclude-table=public.audit_*",
		"--dbname=postgres://repl@s1:5432/appdb?sslmode=prefer",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("pg_dump arguments miss %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "srcsecret") {
		t.Errorf("the password must not be on the command line: %s", joined)
	}
	if len(env) != 1 || env[0] != "PGPASSWORD=srcsecret" {
		t.Errorf("the password must travel in PGPASSWORD, got %v", env)
	}
}

func TestRestoreArgsPreData(t *testing.T) {
	args, env, err := newTestRunner(t).restoreArgs("/tmp/x/schema.dump", PreData, 0)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--section=pre-data", "--exit-on-error", "--single-transaction", "--dbname=host=s3 port=5432 user=postgres dbname=appdb"} {
		if !strings.Contains(joined, want) {
			t.Errorf("pre-data arguments miss %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "--jobs") || strings.Contains(joined, "--clean") {
		t.Errorf("pre-data must be serial and not clean: %s", joined)
	}
	if strings.Contains(joined, "dstsecret") || len(env) != 1 || env[0] != "PGPASSWORD=dstsecret" {
		t.Errorf("password handling wrong: args=%s env=%v", joined, env)
	}
	if args[len(args)-1] != "/tmp/x/schema.dump" {
		t.Errorf("the archive must be the last argument: %v", args)
	}
}

func TestRestoreArgsPostData(t *testing.T) {
	r := newTestRunner(t)

	args, _, err := r.restoreArgs("a.dump", PostData, 4)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--section=post-data") || !strings.Contains(joined, "--jobs=4") {
		t.Errorf("post-data must run in parallel: %s", joined)
	}
	if strings.Contains(joined, "--single-transaction") {
		t.Errorf("post-data must not use a single transaction (incompatible with --jobs): %s", joined)
	}
	if strings.Contains(joined, "--clean") {
		t.Errorf("dropping and recreating fails for inherited partition constraints, existing objects are skipped instead: %s", joined)
	}

	args, _, err = r.restoreArgs("a.dump", PostData, 1)
	if err != nil {
		t.Fatal(err)
	}
	if joined = strings.Join(args, " "); strings.Contains(joined, "--jobs") {
		t.Errorf("one job needs no --jobs: %s", joined)
	}
}
func TestFilterTOC(t *testing.T) {
	toc := strings.Join([]string{
		";",
		"; Archive created at 2026-11-20 15:00:00 UTC",
		";     dbname: appdb",
		";",
		"; Selected TOC Entries:",
		";",
		"2200; 2615 2200 SCHEMA - public pg_database_owner",
		"2201; 2615 16400 SCHEMA - sales postgres",
		"3489; 0 0 COMMENT - SCHEMA public pg_database_owner",
		"212; 1259 16396 TABLE public customers postgres",
		"",
	}, "\n")

	got, skipped := filterTOC(toc, map[string]bool{"public": true})
	if len(skipped) != 1 || skipped[0] != "public" {
		t.Fatalf("skipped = %v, want [public]", skipped)
	}
	lines := strings.Split(got, "\n")
	checks := map[int]string{
		6: ";2200; 2615 2200 SCHEMA - public pg_database_owner",  // creation of an existing schema: commented out
		7: "2201; 2615 16400 SCHEMA - sales postgres",            // another schema stays
		8: "3489; 0 0 COMMENT - SCHEMA public pg_database_owner", // the comment still applies
		9: "212; 1259 16396 TABLE public customers postgres",
	}
	for i, want := range checks {
		if lines[i] != want {
			t.Errorf("line %d = %q, want %q", i, lines[i], want)
		}
	}

	// Nothing to skip leaves the list untouched.
	same, none := filterTOC(toc, nil)
	if same != toc || len(none) != 0 {
		t.Error("an empty skip set must not change the list")
	}
	// A table that happens to be named like a schema is not a schema entry.
	tableOnly := "212; 1259 16396 TABLE public public postgres\n"
	if out, sk := filterTOC(tableOnly, map[string]bool{"public": true}); out != tableOnly || len(sk) != 0 {
		t.Errorf("only SCHEMA entries may be skipped, got %q %v", out, sk)
	}
}

func TestFilterPostData(t *testing.T) {
	toc := strings.Join([]string{
		"; Selected TOC Entries:",
		"3489; 2606 16466 CONSTRAINT public orders orders_pkey postgres",
		"3490; 2606 16467 CONSTRAINT public metrics_2026_01 metrics_2026_01_pkey postgres",
		"3491; 1259 16421 INDEX public orders_customer_idx postgres",
		"3492; 0 0 INDEX ATTACH public metrics_2026_01_pkey ",
		"3493; 2606 16429 FK CONSTRAINT public order_items order_items_order_id_fkey postgres",
		"3494; 2620 16500 TRIGGER public customers audit_trg postgres",
		"3495; 2606 16501 CONSTRAINT public customers customers_email_key postgres",
		"3496; 0 0 MATERIALIZED VIEW DATA public totals postgres",
		"",
	}, "\n")
	ex := &ExistingObjects{
		Indexes:     map[string]bool{"public.orders_customer_idx": true},
		Constraints: map[string]bool{"public.orders.orders_pkey": true, "public.metrics_2026_01.metrics_2026_01_pkey": true, "public.order_items.order_items_order_id_fkey": true},
		Triggers:    map[string]bool{"public.customers.audit_trg": true},
	}

	got, skipped := filterPostData(toc, ex)
	lines := strings.Split(got, "\n")
	commented := map[int]bool{1: true, 2: true, 3: true, 5: true, 6: true} // existing constraint, partition constraint, index, FK, trigger
	for i, line := range lines {
		if commented[i] != strings.HasPrefix(line, ";") && i > 0 {
			t.Errorf("line %d: commented = %v, want %v: %s", i, strings.HasPrefix(line, ";"), commented[i], line)
		}
	}
	if len(skipped) != 5 {
		t.Errorf("skipped = %v, want 5 entries", skipped)
	}
	// What is missing, the index attachment and the materialized view refresh are kept.
	for _, keep := range []string{"customers_email_key", "INDEX ATTACH", "MATERIALIZED VIEW DATA"} {
		for _, line := range lines {
			if strings.Contains(line, keep) && strings.HasPrefix(line, ";") {
				t.Errorf("%q must stay in the restore list: %s", keep, line)
			}
		}
	}

	// Nothing exists: nothing is removed.
	same, none := filterPostData(toc, &ExistingObjects{})
	if same != toc || len(none) != 0 {
		t.Error("an empty target must leave the list untouched")
	}
}

func TestTail(t *testing.T) {
	if got := tail("  short  ", 100); got != "short" {
		t.Errorf("tail = %q", got)
	}
	if got := tail(strings.Repeat("x", 50), 10); got != "..."+strings.Repeat("x", 10) {
		t.Errorf("tail = %q", got)
	}
}

func TestTempFileCleanup(t *testing.T) {
	path, cleanup, err := TempFile()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "schema.dump") {
		t.Errorf("unexpected path %q", path)
	}
	cleanup()
}
