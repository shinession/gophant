package precheck

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/shinession/gophant/internal/catalog"
	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/format"
	"github.com/shinession/gophant/internal/meta"
)

// This file holds the evaluation half of every check: pure functions that turn facts read from the
// databases into a Result. They are unit tested without a database; checker.go gathers the facts.

func pass(id, format string, args ...any) Result {
	return Result{ID: id, Severity: SeverityPass, Summary: fmt.Sprintf(format, args...)}
}

func warn(id, hint, format string, args ...any) Result {
	return Result{ID: id, Severity: SeverityWarn, Summary: fmt.Sprintf(format, args...), Hint: hint}
}

func fail(id, hint, format string, args ...any) Result {
	return Result{ID: id, Severity: SeverityFail, Summary: fmt.Sprintf(format, args...), Hint: hint}
}

func major(versionNum int) int { return versionNum / 10000 }

// evalVersions checks the server versions of s1 and s3.
func evalVersions(src, dst int) []Result {
	var out []Result
	if major(src) < 13 {
		out = append(out, fail("source.version",
			"upgrade s1, publish_via_partition_root needs PostgreSQL 13 or later",
			"source is PostgreSQL %d, logical replication of partitioned tables needs 13 or later", major(src)))
	} else {
		out = append(out, pass("source.version", "source is PostgreSQL %d", major(src)))
	}
	switch {
	case major(dst) < major(src):
		out = append(out, fail("target.version",
			"use a target that is at least as new as the source",
			"target is PostgreSQL %d, older than the source (%d)", major(dst), major(src)))
	case major(dst) != 18:
		out = append(out, warn("target.version",
			"gophant is designed for a PostgreSQL 18 target",
			"target is PostgreSQL %d, not 18", major(dst)))
	default:
		out = append(out, pass("target.version", "target is PostgreSQL %d", major(dst)))
	}
	return out
}

func evalWALLevel(value string) Result {
	if value == "logical" {
		return pass("source.wal_level", "wal_level = logical")
	}
	return fail("source.wal_level",
		"set wal_level = logical in postgresql.conf and restart s1 once",
		"wal_level = %s, logical decoding needs logical", value)
}

func evalPrivileges(superuser, replication bool, noSelectTables, noSelectSeqs []string) []Result {
	var out []Result
	if superuser || replication {
		out = append(out, pass("source.replication_role", "the source role may use the replication protocol"))
	} else {
		out = append(out, fail("source.replication_role",
			"ALTER ROLE <role> REPLICATION (and GRANT pg_read_all_data TO <role>)",
			"the source role has neither the REPLICATION attribute nor superuser"))
	}
	if len(noSelectTables) == 0 {
		out = append(out, pass("source.read_privilege", "the source role can read every table in scope"))
	} else {
		r := fail("source.read_privilege",
			"GRANT pg_read_all_data TO <role>, or GRANT SELECT on these tables",
			"the source role cannot read %d table(s) in scope", len(noSelectTables))
		r.Items = noSelectTables
		out = append(out, r)
	}
	if len(noSelectSeqs) == 0 {
		out = append(out, pass("source.sequence_privilege", "the source role can read every sequence in scope"))
	} else {
		// pg_sequences.last_value is NULL without the privilege, so cutover would silently skip the
		// sequence and the target would hand out values that already exist.
		r := fail("source.sequence_privilege",
			"GRANT pg_read_all_data TO <role>, or GRANT SELECT on these sequences",
			"the source role cannot read %d sequence(s); their values could not be synchronized at cutover", len(noSelectSeqs))
		r.Items = noSelectSeqs
		out = append(out, r)
	}
	return out
}

// evalSlotKeepSize checks max_slot_wal_keep_size, given in MB (-1 means unlimited).
func evalSlotKeepSize(mb int64, allowUnlimited bool) Result {
	if mb > 0 {
		return pass("source.max_slot_wal_keep_size", "max_slot_wal_keep_size = %d MB", mb)
	}
	const hint = "set max_slot_wal_keep_size (for example '30GB', 30%-50% of the free disk) and run SELECT pg_reload_conf();"
	desc := "unlimited (-1)"
	if mb == 0 {
		desc = "0"
	}
	if allowUnlimited {
		return warn("source.max_slot_wal_keep_size", hint,
			"max_slot_wal_keep_size is %s: a stalled slot can fill the disk of s1 (allowed by precheck.allow_unlimited_slot_wal)", desc)
	}
	return fail("source.max_slot_wal_keep_size", hint,
		"max_slot_wal_keep_size is %s: a stalled slot could fill the disk of s1", desc)
}

func evalCapacity(maxSlots, usedSlots, maxSenders, usedSenders int, ownSlot bool) Result {
	freeSlots, freeSenders := maxSlots-usedSlots, maxSenders-usedSenders
	if (freeSlots < 1 && !ownSlot) || freeSenders < 1 {
		return fail("source.replication_capacity",
			"raise max_replication_slots / max_wal_senders (restart required) or drop an unused slot",
			"no free replication capacity: slots %d/%d, walsenders %d/%d", usedSlots, maxSlots, usedSenders, maxSenders)
	}
	return pass("source.replication_capacity", "slots %d/%d used, walsenders %d/%d used", usedSlots, maxSlots, usedSenders, maxSenders)
}

// publicationInfo is what pg_publication says about the configured publication.
type publicationInfo struct {
	Exists                                        bool
	AllTables                                     bool
	Insert, Update, Delete, Truncate, ViaPartRoot bool
	// MissingTables are the tables in scope that the publication does not cover.
	MissingTables []string
}

func evalPublication(name string, p publicationInfo) Result {
	id := "source.publication"
	if !p.Exists {
		return fail(id,
			fmt.Sprintf("as a superuser run: CREATE PUBLICATION %s FOR ALL TABLES WITH (publish_via_partition_root = true);", name),
			"publication %s does not exist", name)
	}
	var problems []string
	if !p.ViaPartRoot {
		problems = append(problems, "publish_via_partition_root is off (partition changes would use the leaf table name)")
	}
	var ops []string
	for _, o := range []struct {
		on   bool
		name string
	}{{p.Insert, "insert"}, {p.Update, "update"}, {p.Delete, "delete"}, {p.Truncate, "truncate"}} {
		if !o.on {
			ops = append(ops, o.name)
		}
	}
	if len(ops) > 0 {
		problems = append(problems, "does not publish "+strings.Join(ops, ", "))
	}
	if len(p.MissingTables) > 0 {
		problems = append(problems, fmt.Sprintf("does not cover %d table(s) in scope", len(p.MissingTables)))
	}
	if len(problems) == 0 {
		scope := "FOR ALL TABLES"
		if !p.AllTables {
			scope = "for the tables in scope"
		}
		return pass(id, "publication %s publishes insert, update, delete and truncate via the partition root (%s)", name, scope)
	}
	r := fail(id,
		fmt.Sprintf("fix it, for example: ALTER PUBLICATION %s SET (publish = 'insert, update, delete, truncate', publish_via_partition_root = true);", name),
		"publication %s is not usable: %s", name, strings.Join(problems, "; "))
	r.Items = p.MissingTables
	return r
}

// slotInfo is what pg_replication_slots says about the configured slot.
type slotInfo struct {
	Exists    bool
	Active    bool
	Plugin    string
	WALStatus string
}

func evalSlot(name string, s slotInfo, state *meta.SyncState) Result {
	id := "source.slot"
	tracked := state != nil && state.SlotName == name
	switch {
	case !s.Exists && tracked && state.Stage != meta.StagePrecheck:
		return fail(id, "the migration cannot continue without its slot: run snapshot --restart",
			"slot %s no longer exists but the migration is in stage %s", name, state.Stage)
	case !s.Exists:
		return pass(id, "slot %s does not exist yet (the snapshot stage creates it)", name)
	case !tracked:
		return fail(id,
			fmt.Sprintf("drop it with SELECT pg_drop_replication_slot('%s') or choose another source.slot_name", name),
			"slot %s already exists but is not tracked by this migration (stale slot)", name)
	case s.WALStatus == "lost":
		return fail(id, "WAL needed by the slot was removed (max_slot_wal_keep_size exceeded): run snapshot --restart",
			"slot %s is invalidated (wal_status = lost)", name)
	case s.Plugin != "pgoutput":
		return fail(id, "drop the slot and let the snapshot stage create it again",
			"slot %s uses plugin %q, gophant needs pgoutput", name, s.Plugin)
	}
	return pass(id, "slot %s exists (wal_status = %s, active = %t)", name, s.WALStatus, s.Active)
}

func evalScope(tables []catalog.Table, missingSchemas []string) Result {
	physical := 0
	var bytes int64
	for _, t := range tables {
		if t.IsPhysical() {
			physical++
			bytes += t.TotalBytes
		}
	}
	if len(tables) == 0 {
		return fail("source.scope", "check filter.include_schemas and filter.exclude_tables",
			"the filter selects no table")
	}
	if len(missingSchemas) > 0 {
		r := warn("source.scope", "fix the schema names in filter.include_schemas",
			"%d schema(s) of filter.include_schemas do not exist on s1", len(missingSchemas))
		r.Items = missingSchemas
		return r
	}
	return pass("source.scope", "%d table(s) in scope, %d of them hold rows, about %s", len(tables), physical, format.Bytes(bytes))
}

// evalReplicaIdentity finds the tables whose UPDATE and DELETE cannot be replicated.
//
// A table without a usable replica identity makes UPDATE and DELETE fail on s1 as soon as a
// publication that covers it exists, so this is a FAIL, not just a note. Partitions are skipped:
// with publish_via_partition_root the root decides.
func evalReplicaIdentity(tables []catalog.Table) Result {
	id := "source.replica_identity"
	var failItems, warnItems []string
	checked := 0
	for _, t := range tables {
		if t.IsPartition {
			continue
		}
		checked++
		switch t.ReplicaIdent {
		case catalog.ReplicaFull:
			if !t.HasPK {
				warnItems = append(warnItems, t.FullName()+": REPLICA IDENTITY FULL without primary key (every UPDATE/DELETE scans the table; duplicate rows are handled one at a time)")
			}
		case catalog.ReplicaIndex:
			if !t.HasReplicaIndex {
				failItems = append(failItems, t.FullName()+": REPLICA IDENTITY USING INDEX, but no index is marked as the identity")
			}
		case catalog.ReplicaNothing:
			failItems = append(failItems, t.FullName()+": REPLICA IDENTITY NOTHING")
		default:
			if !t.HasPK {
				failItems = append(failItems, t.FullName()+": no primary key and the default replica identity")
			}
		}
	}
	const hint = "add a primary key, or ALTER TABLE <table> REPLICA IDENTITY FULL (or USING INDEX <unique not-null index>); " +
		"until then UPDATE and DELETE on the table are rejected on s1 once the publication exists"
	switch {
	case len(failItems) > 0:
		r := fail(id, hint, "%d table(s) cannot replicate UPDATE/DELETE", len(failItems))
		r.Items = append(failItems, warnItems...)
		return r
	case len(warnItems) > 0:
		r := warn(id, "add a primary key to avoid a table scan per UPDATE/DELETE", "%d table(s) rely on REPLICA IDENTITY FULL without a primary key", len(warnItems))
		r.Items = warnItems
		return r
	}
	return pass(id, "all %d table(s) have a usable replica identity", checked)
}

func evalUnlogged(tables []catalog.Table) Result {
	id := "source.unlogged_tables"
	var items []string
	failed := false
	for _, t := range tables {
		if t.Persistence != 'u' {
			continue
		}
		if t.Kind == catalog.KindPartitioned {
			failed = true
			items = append(items, t.FullName()+" (partitioned: PostgreSQL 18 rejects UNLOGGED partitioned tables)")
		} else {
			items = append(items, t.FullName())
		}
	}
	switch {
	case failed:
		r := fail(id, "ALTER TABLE <table> SET LOGGED before the migration", "UNLOGGED tables that cannot be restored on PostgreSQL 18")
		r.Items = items
		return r
	case len(items) > 0:
		r := warn(id, "their current rows are copied but later changes are not in the WAL: ALTER TABLE <table> SET LOGGED, or accept the loss",
			"%d UNLOGGED table(s): logical decoding does not see their changes", len(items))
		r.Items = items
		return r
	}
	return pass(id, "no UNLOGGED tables")
}

func evalGeneratedColumns(tables []catalog.Table) Result {
	id := "source.generated_columns"
	var items []string
	for _, t := range tables {
		for _, c := range t.Columns {
			switch c.Generated {
			case 's':
				items = append(items, fmt.Sprintf("%s.%s (stored)", t.FullName(), c.Name))
			case 'v':
				items = append(items, fmt.Sprintf("%s.%s (virtual)", t.FullName(), c.Name))
			}
		}
	}
	if len(items) == 0 {
		return pass(id, "no generated columns")
	}
	r := warn(id, "nothing to do: gophant leaves them out of COPY and replay and the target computes them",
		"%d generated column(s) are excluded from COPY and replay", len(items))
	r.Items = items
	return r
}

func evalMD5Roles(roles []string, unreadable bool) Result {
	id := "source.md5_passwords"
	switch {
	case unreadable:
		return warn(id, "run precheck with a superuser to check pg_authid",
			"cannot read pg_authid, md5 password hashes were not checked")
	case len(roles) > 0:
		r := warn(id, "MD5 is deprecated in PostgreSQL 18: reset these passwords with password_encryption = 'scram-sha-256'",
			"%d role(s) still use an md5 password hash", len(roles))
		r.Items = roles
		return r
	}
	return pass(id, "no role uses an md5 password hash")
}

// extInfo describes one extension installed on s1.
type extInfo struct {
	Name, Version, Schema string
}

func evalExtensions(src []extInfo, dstInstalled map[string]string, dstAvailable map[string]bool, f *config.Filter, skipDDL bool) Result {
	id := "target.extensions"
	var failItems, warnItems []string
	checked := 0
	for _, e := range src {
		if e.Name == "plpgsql" {
			continue
		}
		checked++
		if v, ok := dstInstalled[e.Name]; ok {
			if v != e.Version {
				warnItems = append(warnItems, fmt.Sprintf("%s: version %s on s1, %s on s3", e.Name, e.Version, v))
			}
			continue
		}
		switch {
		case !dstAvailable[e.Name]:
			failItems = append(failItems, fmt.Sprintf("%s: not available on s3, install its package for PostgreSQL 18", e.Name))
		case skipDDL:
			failItems = append(failItems, fmt.Sprintf("%s: available but not installed on s3 and snapshot.skip_ddl is true: CREATE EXTENSION %s SCHEMA %s", e.Name, e.Name, e.Schema))
		case !f.MatchSchema(e.Schema):
			failItems = append(failItems, fmt.Sprintf("%s: lives in schema %s, which is outside filter.include_schemas, so pg_dump will not create it: CREATE EXTENSION %s SCHEMA %s on s3 first", e.Name, e.Schema, e.Name, e.Schema))
		}
	}
	switch {
	case len(failItems) > 0:
		r := fail(id, "make every extension of s1 available on s3", "%d extension(s) of s1 are missing on s3", len(failItems))
		r.Items = append(failItems, warnItems...)
		return r
	case len(warnItems) > 0:
		r := warn(id, "align the extension versions unless the difference is intended", "%d extension(s) differ in version", len(warnItems))
		r.Items = warnItems
		return r
	}
	return pass(id, "all %d extension(s) of s1 are available on s3", checked)
}

func evalRoles(needed []string, existing map[string]bool) Result {
	id := "target.roles"
	var missing []string
	for _, r := range needed {
		if !existing[r] {
			missing = append(missing, r)
		}
	}
	if len(missing) == 0 {
		return pass(id, "all %d role(s) that own or are granted objects exist on s3", len(needed))
	}
	sort.Strings(missing)
	r := fail(id, "create the roles on s3 first, for example with pg_dumpall --roles-only | psql (roles are not part of a database dump)",
		"%d role(s) used by objects of s1 do not exist on s3", len(missing))
	r.Items = missing
	return r
}

// evalTargetState checks that s3 is in the state the snapshot stage expects.
//
// With snapshot.skip_ddl the tables have to exist and must be empty: the copy adds the rows of the
// snapshot to whatever is there, so rows restored from a backup beforehand would be duplicated (or,
// with a primary key already in place, stop the copy). nonEmpty lists the tables that hold rows.
func evalTargetState(tables []catalog.Table, existsOnTarget map[string]bool, skipDDL bool, state *meta.SyncState, nonEmpty []string) Result {
	id := "target.state"
	if state != nil && state.Stage != meta.StagePrecheck {
		return pass(id, "migration state found on s3, stage %s (the objects of s3 are managed by this migration)", state.Stage)
	}
	var wrong []string
	for _, t := range tables {
		present := existsOnTarget[t.FullName()]
		if skipDDL && !present {
			wrong = append(wrong, t.FullName()+": missing on s3")
		}
		if !skipDDL && present {
			wrong = append(wrong, t.FullName()+": already exists on s3")
		}
	}
	if len(wrong) == 0 {
		if skipDDL && len(nonEmpty) > 0 {
			r := fail(id, "gophant copies into empty tables only, the rows would be duplicated: TRUNCATE these tables (or load the schema without data) and run precheck again",
				"%d table(s) on s3 already contain rows", len(nonEmpty))
			r.Items = nonEmpty
			return r
		}
		if skipDDL {
			return pass(id, "every table in scope exists on s3 and is empty (snapshot.skip_ddl = true)")
		}
		return pass(id, "s3 holds none of the tables in scope")
	}
	if skipDDL {
		r := fail(id, "create the schema on s3 first, or set snapshot.skip_ddl = false", "%d table(s) are missing on s3", len(wrong))
		r.Items = wrong
		return r
	}
	r := fail(id, "start from an empty database: the pre-data import creates these tables (drop them, or set snapshot.skip_ddl = true if the schema was prepared on purpose)",
		"s3 already contains %d table(s) in scope", len(wrong))
	r.Items = wrong
	return r
}

func evalTool(name string, toolMajor, srcMajor int, err error) Result {
	id := "tools." + name
	switch {
	case err != nil:
		return fail(id, "set tools."+name+"_bin in the configuration to the PostgreSQL 18 binary",
			"%s is not usable: %v", name, err)
	case toolMajor < srcMajor:
		return fail(id, "use a pg_dump/pg_restore that is at least as new as the source server",
			"%s is version %d, older than the source server (%d)", name, toolMajor, srcMajor)
	case toolMajor != 18:
		return warn(id, "PostgreSQL 18 client tools are recommended", "%s is version %d, not 18", name, toolMajor)
	}
	return pass(id, "%s is version %d", name, toolMajor)
}

// dbEncoding is the encoding and collation of one database.
type dbEncoding struct {
	Encoding, Collate, Ctype string
}

func evalEncoding(src, dst dbEncoding) Result {
	id := "target.encoding"
	var diffs []string
	if src.Encoding != dst.Encoding {
		diffs = append(diffs, fmt.Sprintf("encoding %s on s1, %s on s3", src.Encoding, dst.Encoding))
	}
	if src.Collate != dst.Collate {
		diffs = append(diffs, fmt.Sprintf("LC_COLLATE %s on s1, %s on s3", src.Collate, dst.Collate))
	}
	if src.Ctype != dst.Ctype {
		diffs = append(diffs, fmt.Sprintf("LC_CTYPE %s on s1, %s on s3", src.Ctype, dst.Ctype))
	}
	if len(diffs) == 0 {
		return pass(id, "encoding %s and collation %s match", src.Encoding, src.Collate)
	}
	r := warn(id, "create the target database with the same encoding and collation, otherwise sort order and unique indexes can differ",
		"the databases differ in encoding or collation")
	r.Items = diffs
	return r
}

// evalMonitoring checks that the source role can see the transactions of other sessions. The cutover
// uses that to refuse to go on while a session still holds a write transaction.
func evalMonitoring(canSee bool) Result {
	id := "source.monitoring"
	if canSee {
		return pass(id, "the source role can see the sessions of other users")
	}
	return warn(id, "GRANT pg_read_all_stats TO <role>; otherwise the cutover cannot detect open write transactions on s1",
		"the source role cannot see the transactions of other users' sessions")
}

func evalLargeObjects(count int64) Result {
	id := "source.large_objects"
	if count == 0 {
		return pass(id, "no large objects")
	}
	return warn(id, "large objects are not part of the WAL stream or of the schema dump: migrate them separately or move them to bytea",
		"%d large object(s) exist and will NOT be migrated", count)
}

func evalBinaryCopy(fallbacks []catalog.FormatFallback) Result {
	id := "source.copy_format"
	if len(fallbacks) == 0 {
		return pass(id, "every table can be copied in binary format")
	}
	items := make([]string, len(fallbacks))
	for i, f := range fallbacks {
		items[i] = fmt.Sprintf("%s.%s (%s)", f.Table, f.Column, f.Type)
	}
	r := warn(id, "nothing to do: these tables are copied in text format, which is slower but portable",
		"%d table(s) have a column type that is not portable in binary COPY", len(fallbacks))
	r.Items = items
	return r
}

func evalLongTransactions(count int64, oldest time.Duration) Result {
	id := "source.long_transactions"
	if count == 0 {
		return pass(id, "no long running write transaction")
	}
	return warn(id, "creating the replication slot waits until these transactions end; finish or cancel them to avoid a long wait",
		"%d write transaction(s) have been open for more than 5 minutes (oldest %s)", count, oldest.Round(time.Second))
}

// sortedKeys returns the keys of a set in order, for stable output.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
