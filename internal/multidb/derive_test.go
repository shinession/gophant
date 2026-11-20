package multidb

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/shinession/gophant/internal/config"
)

const baseYAML = `
source:
  dsn: "postgres://gophant_repl:pa%24%24@s1:5432/postgres?sslmode=prefer"
  replication_dsn: "postgres://gophant_repl:pa%24%24@s1:5432/postgres?sslmode=prefer&replication=database"
  slot_name: "gophant_slot"
  publication_name: "gophant_pub"
target:
  dsn: "postgres://postgres:s3cret@s3:5433/postgres?sslmode=prefer"
filter:
  include_schemas: ["public"]
  exclude_tables: ["public.audit_*"]
snapshot:
  workers: 7
databases:
  auto: true
`

func parse(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	return cfg
}

func TestDatabaseConfigPointsEveryConnectionAtTheDatabase(t *testing.T) {
	base := parse(t, baseYAML)
	got, err := DatabaseConfig(base, "Sales-2024")
	if err != nil {
		t.Fatalf("DatabaseConfig: %v", err)
	}

	for name, dsn := range map[string]string{"source.dsn": got.Source.DSN, "source.replication_dsn": got.Source.ReplicationDSN, "target.dsn": got.Target.DSN} {
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("%s does not parse: %v", name, err)
		}
		if cfg.Database != "Sales-2024" {
			t.Errorf("%s points at %q", name, cfg.Database)
		}
	}
	src, _ := pgx.ParseConfig(got.Source.DSN)
	dst, _ := pgx.ParseConfig(got.Target.DSN)
	if src.Host != "s1" || src.Password != "pa$$" || dst.Host != "s3" || dst.Port != 5433 || dst.Password != "s3cret" {
		t.Errorf("host, port or password changed: %s %q / %s:%d %q", src.Host, src.Password, dst.Host, dst.Port, dst.Password)
	}
	if !strings.Contains(got.Source.ReplicationDSN, "replication=database") {
		t.Errorf("the replication parameter is gone: %s", got.Source.ReplicationDSN)
	}
	if got.Source.SlotName != SlotName("gophant_slot", "Sales-2024") || got.Source.PublicationName != "gophant_pub" {
		t.Errorf("slot or publication wrong: %q %q", got.Source.SlotName, got.Source.PublicationName)
	}
}

func TestDatabaseConfigKeepsTheOtherSettingsAndTheOriginal(t *testing.T) {
	base := parse(t, baseYAML)
	before := *base
	got, err := DatabaseConfig(base, "crm")
	if err != nil {
		t.Fatalf("DatabaseConfig: %v", err)
	}
	if got.Snapshot.Workers != 7 || got.Target.MetaSchema != "gophant_meta" {
		t.Errorf("settings were not carried over: %+v", got.Snapshot)
	}
	if got.Filter.Match("public", "audit_log") || !got.Filter.Match("public", "orders") {
		t.Error("the table filter of the copy does not work")
	}
	if base.Source != before.Source || base.Target != before.Target {
		t.Errorf("the base configuration was modified: %+v", base.Source)
	}
}

func TestDatabaseConfigWithoutReplicationDSN(t *testing.T) {
	base := parse(t, strings.Replace(baseYAML, `  replication_dsn: "postgres://gophant_repl:pa%24%24@s1:5432/postgres?sslmode=prefer&replication=database"`+"\n", "", 1))
	got, err := DatabaseConfig(base, "crm")
	if err != nil {
		t.Fatalf("DatabaseConfig: %v", err)
	}
	if got.Source.ReplicationDSN != "" {
		t.Errorf("a replication DSN appeared: %q", got.Source.ReplicationDSN)
	}
}

func TestDatabaseConfigErrorsDoNotLeakThePassword(t *testing.T) {
	base := parse(t, baseYAML)
	base.Source.DSN = "postgres://u:topsecret@host:notaport/postgres"
	_, err := DatabaseConfig(base, "crm")
	if err == nil {
		t.Fatal("an invalid DSN must be rejected")
	}
	if strings.Contains(err.Error(), "topsecret") || !strings.Contains(err.Error(), "source.dsn") {
		t.Errorf("unexpected error: %v", err)
	}
}
