// Package config loads, defaults and validates the gophant YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Allowed values of verify.mode.
const (
	VerifyModeCountOnly      = "count_only"
	VerifyModeCountAndSample = "count_and_sample"
	VerifyModeFullHash       = "full_hash"
)

// Config is the root of the gophant YAML configuration file.
type Config struct {
	Source    Source    `yaml:"source"`
	Target    Target    `yaml:"target"`
	Databases Databases `yaml:"databases"`
	Tools     Tools     `yaml:"tools"`
	Filter    Filter    `yaml:"filter"`
	Precheck  Precheck  `yaml:"precheck"`
	Snapshot  Snapshot  `yaml:"snapshot"`
	CDC       CDC       `yaml:"cdc"`
	Cutover   Cutover   `yaml:"cutover"`
	Verify    Verify    `yaml:"verify"`
}

// Source describes the PostgreSQL 16 source database (s1).
type Source struct {
	// DSN is the regular (SQL) connection string.
	DSN string `yaml:"dsn"`
	// ReplicationDSN is the connection string used for the replication protocol.
	// When empty, DSN is used and the replication=database parameter is added.
	ReplicationDSN  string `yaml:"replication_dsn"`
	SlotName        string `yaml:"slot_name"`
	PublicationName string `yaml:"publication_name"`
}

// Target describes the PostgreSQL 18 target database (s3).
type Target struct {
	DSN        string `yaml:"dsn"`
	MetaSchema string `yaml:"meta_schema"`
}

// Databases switches gophant from one pair of databases to every database of the cluster.
//
// With Auto set, the databases are found on s1 and each one is migrated like a configuration of its
// own: the same settings, the database name taken from the discovery, a replication slot named after
// the database. The database named in source.dsn and target.dsn is then only the one gophant
// connects to first (use postgres); the filter, snapshot, cdc, cutover and verify settings apply to
// every database.
type Databases struct {
	// Auto turns the multi-database mode on.
	Auto bool `yaml:"auto"`
	// Include lists the databases to migrate: exact names or patterns with * and ?. Empty means every
	// database that was found.
	Include []string `yaml:"include"`
	// Exclude lists the databases that are never migrated; it wins over Include.
	Exclude []string `yaml:"exclude"`
	// Parallel is the number of databases whose snapshot runs at the same time.
	Parallel int `yaml:"parallel"`
	// CreateTarget creates a database that s3 does not have yet. It defaults to true.
	CreateTarget *bool `yaml:"create_target"`
	// CreatePublication creates the publication in a database of s1 that does not have it, when the
	// source role is a superuser. It defaults to true.
	CreatePublication *bool `yaml:"create_publication"`

	include []*regexp.Regexp
	exclude []*regexp.Regexp
}

// CreatesTarget reports whether a missing database of s3 is created by gophant.
func (d *Databases) CreatesTarget() bool { return d.CreateTarget == nil || *d.CreateTarget }

// CreatesPublication reports whether a missing publication of s1 is created by gophant.
func (d *Databases) CreatesPublication() bool {
	return d.CreatePublication == nil || *d.CreatePublication
}

// Matches reports whether a database passes include and exclude.
func (d *Databases) Matches(name string) bool {
	if slices.ContainsFunc(d.exclude, func(p *regexp.Regexp) bool { return p.MatchString(name) }) {
		return false
	}
	return len(d.include) == 0 || slices.ContainsFunc(d.include, func(p *regexp.Regexp) bool { return p.MatchString(name) })
}

// maxMultiSlotBase is the longest source.slot_name of the multi-database mode: the name of the
// database is appended and a slot name has at most 63 characters.
const maxMultiSlotBase = 40

// Tools locates the external PostgreSQL client binaries (version 18 recommended).
type Tools struct {
	PgDumpBin    string `yaml:"pg_dump_bin"`
	PgRestoreBin string `yaml:"pg_restore_bin"`
}

// Filter selects the objects that are migrated.
type Filter struct {
	// IncludeSchemas lists the schemas to migrate. Empty means every non-system schema.
	IncludeSchemas []string `yaml:"include_schemas"`
	// ExcludeTables lists schema-qualified table patterns (schema.table, with * and ? wildcards).
	ExcludeTables []string `yaml:"exclude_tables"`

	excludes []tablePattern
}

// Precheck tunes the pre-flight checks.
type Precheck struct {
	// AllowUnlimitedSlotWAL downgrades the max_slot_wal_keep_size > 0 requirement from FAIL to WARN.
	AllowUnlimitedSlotWAL bool `yaml:"allow_unlimited_slot_wal"`
}

// Snapshot configures the full copy.
type Snapshot struct {
	// Workers is the number of tables that are copied concurrently.
	Workers int `yaml:"workers"`
	// SkipDDL skips the pg_dump/pg_restore steps; the schema on s3 is prepared manually.
	SkipDDL bool `yaml:"skip_ddl"`
}

// CDC configures WAL consumption and replay.
type CDC struct {
	StandbyStatusInterval Duration `yaml:"standby_status_interval"`
	BatchMaxTxns          int      `yaml:"batch_max_txns"`
	BatchMaxDelay         Duration `yaml:"batch_max_delay"`
	MaxTxMemoryMB         int      `yaml:"max_tx_memory_mb"`
	TempDir               string   `yaml:"temp_dir"`
}

// Cutover configures the downtime window steps.
type Cutover struct {
	SequenceOffset int64    `yaml:"sequence_offset"`
	LagZeroTimeout Duration `yaml:"lag_zero_timeout"`
}

// Verify configures the consistency verification.
type Verify struct {
	Workers     int     `yaml:"workers"`
	ChunkSize   int     `yaml:"chunk_size"`
	Mode        string  `yaml:"mode"`
	SampleRatio float64 `yaml:"sample_ratio"`
}

// Duration is a time.Duration that is written as a string such as "5s" or "200ms" in YAML.
type Duration time.Duration

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

var (
	envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	// PostgreSQL restricts replication slot names to lower-case letters, digits and underscores.
	slotNamePattern = regexp.MustCompile(`^[a-z0-9_]{1,63}$`)
	// Publication and schema names are interpolated into the replication command, so keep them plain.
	identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,62}$`)
)

// Load reads, expands, defaults and validates the configuration file at path.
//
// String values may reference environment variables as ${NAME}; a reference to an unset
// variable is an error so that a missing secret never silently becomes an empty password.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw)
}

// Parse parses configuration bytes. See Load.
func Parse(raw []byte) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	var missing []string
	expandNode(&root, &missing)
	if len(missing) > 0 {
		return nil, fmt.Errorf("config references unset environment variables: %s", strings.Join(uniqueSorted(missing), ", "))
	}

	expanded, err := yaml.Marshal(&root)
	if err != nil {
		return nil, fmt.Errorf("re-encode config: %w", err)
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ApplyDefaults fills unset fields with their documented defaults.
func (c *Config) ApplyDefaults() {
	if c.Source.SlotName == "" {
		c.Source.SlotName = "gophant_slot"
	}
	if c.Source.PublicationName == "" {
		c.Source.PublicationName = "gophant_pub"
	}
	if c.Target.MetaSchema == "" {
		c.Target.MetaSchema = "gophant_meta"
	}
	if c.Databases.Parallel == 0 {
		c.Databases.Parallel = 1
	}
	if c.Tools.PgDumpBin == "" {
		c.Tools.PgDumpBin = "pg_dump"
	}
	if c.Tools.PgRestoreBin == "" {
		c.Tools.PgRestoreBin = "pg_restore"
	}
	if c.Snapshot.Workers == 0 {
		c.Snapshot.Workers = 4
	}
	if c.CDC.StandbyStatusInterval == 0 {
		c.CDC.StandbyStatusInterval = Duration(5 * time.Second)
	}
	if c.CDC.BatchMaxTxns == 0 {
		c.CDC.BatchMaxTxns = 50
	}
	if c.CDC.BatchMaxDelay == 0 {
		c.CDC.BatchMaxDelay = Duration(200 * time.Millisecond)
	}
	if c.CDC.MaxTxMemoryMB == 0 {
		c.CDC.MaxTxMemoryMB = 64
	}
	if c.CDC.TempDir == "" {
		c.CDC.TempDir = filepath.Join(os.TempDir(), "gophant-spill")
	}
	if c.Cutover.SequenceOffset == 0 {
		c.Cutover.SequenceOffset = 100
	}
	if c.Cutover.LagZeroTimeout == 0 {
		c.Cutover.LagZeroTimeout = Duration(5 * time.Minute)
	}
	if c.Verify.Workers == 0 {
		c.Verify.Workers = 4
	}
	if c.Verify.ChunkSize == 0 {
		c.Verify.ChunkSize = 50000
	}
	if c.Verify.Mode == "" {
		c.Verify.Mode = VerifyModeCountAndSample
	}
	if c.Verify.SampleRatio == 0 {
		c.Verify.SampleRatio = 0.1
	}
}

// Validate checks the configuration and reports every problem at once.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Source.DSN == "" {
		add("source.dsn is required")
	}
	if c.Target.DSN == "" {
		add("target.dsn is required")
	}
	if !slotNamePattern.MatchString(c.Source.SlotName) {
		add("source.slot_name %q is invalid: use 1-63 lower-case letters, digits or underscores", c.Source.SlotName)
	}
	if !identPattern.MatchString(c.Source.PublicationName) {
		add("source.publication_name %q is invalid: use letters, digits, underscores or $, not starting with a digit", c.Source.PublicationName)
	}
	if !identPattern.MatchString(c.Target.MetaSchema) {
		add("target.meta_schema %q is invalid: use letters, digits, underscores or $, not starting with a digit", c.Target.MetaSchema)
	}

	if c.Databases.Parallel < 1 {
		add("databases.parallel must be >= 1")
	}
	include, err := compileDatabasePatterns("databases.include", c.Databases.Include)
	if err != nil {
		errs = append(errs, err)
	}
	c.Databases.include = include
	exclude, err := compileDatabasePatterns("databases.exclude", c.Databases.Exclude)
	if err != nil {
		errs = append(errs, err)
	}
	c.Databases.exclude = exclude
	if c.Databases.Auto && len(c.Source.SlotName) > maxMultiSlotBase {
		add("source.slot_name %q is too long for databases.auto: the name of the database is appended, use at most %d characters",
			c.Source.SlotName, maxMultiSlotBase)
	}

	for _, s := range c.Filter.IncludeSchemas {
		if s == "" || strings.ContainsAny(s, "*?") {
			add("filter.include_schemas entry %q is invalid: use exact schema names without wildcards", s)
		}
	}
	excludes, err := compileTablePatterns(c.Filter.ExcludeTables)
	if err != nil {
		errs = append(errs, err)
	}
	c.Filter.excludes = excludes

	if c.Snapshot.Workers < 1 {
		add("snapshot.workers must be >= 1")
	}
	if c.CDC.StandbyStatusInterval.Std() <= 0 {
		add("cdc.standby_status_interval must be > 0")
	}
	if c.CDC.BatchMaxTxns < 1 {
		add("cdc.batch_max_txns must be >= 1")
	}
	if c.CDC.BatchMaxDelay.Std() <= 0 {
		add("cdc.batch_max_delay must be > 0")
	}
	if c.CDC.MaxTxMemoryMB < 1 {
		add("cdc.max_tx_memory_mb must be >= 1")
	}
	if c.Cutover.SequenceOffset < 0 {
		add("cutover.sequence_offset must be >= 0")
	}
	if c.Cutover.LagZeroTimeout.Std() <= 0 {
		add("cutover.lag_zero_timeout must be > 0")
	}
	if c.Verify.Workers < 1 {
		add("verify.workers must be >= 1")
	}
	if c.Verify.ChunkSize < 1 {
		add("verify.chunk_size must be >= 1")
	}
	switch c.Verify.Mode {
	case VerifyModeCountOnly, VerifyModeCountAndSample, VerifyModeFullHash:
	default:
		add("verify.mode %q is invalid: use %s, %s or %s", c.Verify.Mode,
			VerifyModeCountOnly, VerifyModeCountAndSample, VerifyModeFullHash)
	}
	if c.Verify.SampleRatio <= 0 || c.Verify.SampleRatio > 1 {
		add("verify.sample_ratio must be in (0, 1]")
	}

	return errors.Join(errs...)
}

// MaxTxMemoryBytes returns cdc.max_tx_memory_mb in bytes.
func (c *Config) MaxTxMemoryBytes() int64 { return int64(c.CDC.MaxTxMemoryMB) << 20 }

// Match reports whether the table schema.name is part of the migration.
func (f *Filter) Match(schema, table string) bool {
	if !f.MatchSchema(schema) {
		return false
	}
	for _, p := range f.excludes {
		if p.match(schema, table) {
			return false
		}
	}
	return true
}

// MatchSchema reports whether the schema is part of the migration.
func (f *Filter) MatchSchema(schema string) bool {
	return len(f.IncludeSchemas) == 0 || slices.Contains(f.IncludeSchemas, schema)
}

// tablePattern is a compiled schema.table exclusion pattern.
type tablePattern struct {
	schema *regexp.Regexp
	table  *regexp.Regexp
}

func (p tablePattern) match(schema, table string) bool {
	return p.schema.MatchString(schema) && p.table.MatchString(table)
}

func compileTablePatterns(patterns []string) ([]tablePattern, error) {
	var out []tablePattern
	var errs []error
	for _, raw := range patterns {
		schemaPart, tablePart, ok := strings.Cut(raw, ".")
		if !ok || schemaPart == "" || tablePart == "" || strings.Contains(tablePart, ".") {
			errs = append(errs, fmt.Errorf("filter.exclude_tables entry %q is invalid: use schema.table (wildcards * and ? allowed)", raw))
			continue
		}
		out = append(out, tablePattern{schema: globToRegexp(schemaPart), table: globToRegexp(tablePart)})
	}
	return out, errors.Join(errs...)
}

// compileDatabasePatterns compiles the include or exclude list of the databases section.
func compileDatabasePatterns(key string, patterns []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	var errs []error
	for _, p := range patterns {
		if p == "" {
			errs = append(errs, fmt.Errorf("%s has an empty entry: use a database name or a pattern with * and ?", key))
			continue
		}
		out = append(out, globToRegexp(p))
	}
	return out, errors.Join(errs...)
}

// globToRegexp converts a pattern with * and ? wildcards into an anchored regular expression.
func globToRegexp(glob string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range glob {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// expandNode expands ${NAME} references in every scalar string of the YAML tree.
func expandNode(n *yaml.Node, missing *[]string) {
	if n.Kind == yaml.ScalarNode && (n.Tag == "!!str" || n.Tag == "") {
		n.Value = envRefPattern.ReplaceAllStringFunc(n.Value, func(ref string) string {
			name := envRefPattern.FindStringSubmatch(ref)[1]
			v, ok := os.LookupEnv(name)
			if !ok {
				*missing = append(*missing, name)
				return ref
			}
			return v
		})
	}
	for _, child := range n.Content {
		expandNode(child, missing)
	}
}

func uniqueSorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}
