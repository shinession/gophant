package config

import (
	"strings"
	"testing"
	"time"
)

const minimalYAML = `
source:
  dsn: "postgres://u:p@s1:5432/appdb"
target:
  dsn: "postgres://u:p@s3:5432/appdb"
`

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Source.SlotName != "gophant_slot" || cfg.Source.PublicationName != "gophant_pub" {
		t.Errorf("slot/publication defaults wrong: %q %q", cfg.Source.SlotName, cfg.Source.PublicationName)
	}
	if cfg.Target.MetaSchema != "gophant_meta" {
		t.Errorf("meta schema default wrong: %q", cfg.Target.MetaSchema)
	}
	if cfg.Snapshot.Workers != 4 || cfg.Verify.Workers != 4 {
		t.Errorf("workers defaults wrong: %d %d", cfg.Snapshot.Workers, cfg.Verify.Workers)
	}
	if cfg.CDC.StandbyStatusInterval.Std() != 5*time.Second || cfg.CDC.BatchMaxDelay.Std() != 200*time.Millisecond {
		t.Errorf("cdc duration defaults wrong: %v %v", cfg.CDC.StandbyStatusInterval.Std(), cfg.CDC.BatchMaxDelay.Std())
	}
	if cfg.Cutover.SequenceOffset != 100 || cfg.Cutover.LagZeroTimeout.Std() != 5*time.Minute {
		t.Errorf("cutover defaults wrong: %d %v", cfg.Cutover.SequenceOffset, cfg.Cutover.LagZeroTimeout.Std())
	}
	if cfg.Verify.Mode != VerifyModeCountAndSample || cfg.Verify.ChunkSize != 50000 || cfg.Verify.SampleRatio != 0.1 {
		t.Errorf("verify defaults wrong: %+v", cfg.Verify)
	}
	if cfg.CDC.TempDir == "" {
		t.Error("cdc.temp_dir default must not be empty")
	}
	if cfg.MaxTxMemoryBytes() != 64<<20 {
		t.Errorf("MaxTxMemoryBytes = %d", cfg.MaxTxMemoryBytes())
	}
}

func TestParseExpandsEnvironmentVariables(t *testing.T) {
	t.Setenv("GOPHANT_TEST_PASSWORD", "s3cret")
	cfg, err := Parse([]byte(`
source:
  dsn: "postgres://u:${GOPHANT_TEST_PASSWORD}@s1:5432/appdb"
target:
  dsn: "postgres://u:p@s3:5432/appdb"
# a comment that mentions ${NOT_SET_IN_A_COMMENT} must not be expanded
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := "postgres://u:s3cret@s1:5432/appdb"; cfg.Source.DSN != want {
		t.Errorf("DSN = %q, want %q", cfg.Source.DSN, want)
	}
}

func TestParseRejectsUnsetEnvironmentVariable(t *testing.T) {
	_, err := Parse([]byte(`
source:
  dsn: "postgres://u:${GOPHANT_DEFINITELY_UNSET}@s1:5432/appdb"
target:
  dsn: "postgres://u:p@s3:5432/appdb"
`))
	if err == nil || !strings.Contains(err.Error(), "GOPHANT_DEFINITELY_UNSET") {
		t.Fatalf("expected unset variable error, got %v", err)
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	_, err := Parse([]byte(minimalYAML + "snapshott:\n  workers: 2\n"))
	if err == nil {
		t.Fatal("expected an error for an unknown top-level key")
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte(`
source:
  dsn: ""
  slot_name: "Bad-Slot"
target:
  dsn: ""
snapshot:
  workers: -1
verify:
  mode: "everything"
  sample_ratio: 2
filter:
  include_schemas: ["pub*"]
  exclude_tables: ["no_schema_here"]
`))
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{
		"source.dsn is required", "target.dsn is required", "source.slot_name",
		"snapshot.workers", "verify.mode", "verify.sample_ratio",
		"filter.include_schemas", "filter.exclude_tables",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestDurationRejectsGarbage(t *testing.T) {
	_, err := Parse([]byte(minimalYAML + "cdc:\n  batch_max_delay: soon\n"))
	if err == nil {
		t.Fatal("expected an error for an invalid duration")
	}
}

func TestFilterMatch(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML + `
filter:
  include_schemas: ["public", "sales"]
  exclude_tables: ["public.audit_*", "sales.tmp?", "public.Exact"]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cases := []struct {
		schema, table string
		want          bool
	}{
		{"public", "orders", true},
		{"sales", "invoices", true},
		{"hr", "people", false},        // schema not included
		{"public", "audit_log", false}, // wildcard exclusion
		{"public", "audit_", false},    // * matches empty
		{"sales", "tmp1", false},       // ? matches one character
		{"sales", "tmp", true},         // ? needs one character
		{"sales", "tmp12", true},       // ? matches exactly one character
		{"public", "Exact", false},     // exact exclusion
		{"public", "exact", true},      // matching is case sensitive
		{"sales", "audit_log", true},   // exclusion is schema specific
	}
	for _, c := range cases {
		if got := cfg.Filter.Match(c.schema, c.table); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.schema, c.table, got, c.want)
		}
	}
}

// The example file is what operators copy, so it must always load.
func TestExampleConfigurationIsValid(t *testing.T) {
	t.Setenv("S1_PASSWORD", "x")
	t.Setenv("S3_PASSWORD", "y")
	cfg, err := Load("../../configs/gophant.example.yaml")
	if err != nil {
		t.Fatalf("the example configuration does not load: %v", err)
	}
	if cfg.Source.SlotName != "gophant_slot" || cfg.Snapshot.Workers != 4 || cfg.Verify.Mode != VerifyModeCountAndSample {
		t.Errorf("unexpected values in the example: %+v", cfg)
	}
	if cfg.Source.DSN == "" || !strings.Contains(cfg.Source.DSN, "x@") {
		t.Errorf("the password placeholder was not expanded: %q", cfg.Source.DSN)
	}
}

func TestDatabasesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	d := &cfg.Databases
	if d.Auto {
		t.Error("the multi-database mode must be off unless it is asked for")
	}
	if d.Parallel != 1 || !d.CreatesTarget() || !d.CreatesPublication() {
		t.Errorf("defaults wrong: parallel=%d create_target=%v create_publication=%v", d.Parallel, d.CreatesTarget(), d.CreatesPublication())
	}
	if !d.Matches("anything") {
		t.Error("without include and exclude every database matches")
	}
}

func TestDatabasesSection(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML + `
databases:
  auto: true
  include: ["app*", "Sales-2024", "crm"]
  exclude: ["app_test*", "crm"]
  parallel: 3
  create_target: false
  create_publication: false
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	d := &cfg.Databases
	if !d.Auto || d.Parallel != 3 || d.CreatesTarget() || d.CreatesPublication() {
		t.Errorf("explicit values were not kept: %+v", d)
	}
	cases := map[string]bool{
		"appdb":      true,
		"app":        true,  // * matches empty
		"app_test1":  false, // exclusion wins over inclusion
		"Sales-2024": true,
		"sales-2024": false, // matching is case sensitive
		"crm":        false, // excluded although included
		"postgres":   false, // not in the include list
	}
	for name, want := range cases {
		if got := d.Matches(name); got != want {
			t.Errorf("Matches(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestDatabasesExcludeOnlyKeepsTheRest(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML + "databases:\n  auto: true\n  exclude: [\"postgres\", \"tmp_?\"]\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for name, want := range map[string]bool{"appdb": true, "postgres": false, "tmp_1": false, "tmp_12": true} {
		if got := cfg.Databases.Matches(name); got != want {
			t.Errorf("Matches(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestDatabasesValidation(t *testing.T) {
	long := strings.Repeat("s", maxMultiSlotBase+1)
	_, err := Parse([]byte(`
source:
  dsn: "postgres://u:p@s1:5432/postgres"
  slot_name: "` + long + `"
target:
  dsn: "postgres://u:p@s3:5432/postgres"
databases:
  auto: true
  parallel: -2
  include: [""]
  exclude: [""]
`))
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{"databases.parallel", "databases.include", "databases.exclude", "source.slot_name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}

	// The slot name only has to leave room for the database name when the mode is on.
	if _, err := Parse([]byte(`
source:
  dsn: "postgres://u:p@s1:5432/appdb"
  slot_name: "` + long + `"
target:
  dsn: "postgres://u:p@s3:5432/appdb"
`)); err != nil {
		t.Errorf("a long slot name is fine for one database: %v", err)
	}
}

func TestFilterWithoutIncludeSchemasMatchesEverything(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.Filter.Match("anything", "goes") || !cfg.Filter.MatchSchema("anything") {
		t.Error("an empty filter must match every table")
	}
}
