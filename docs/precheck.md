# English

## Precheck

`gophant precheck` inspects s1 and s3 and prints a PASS / WARN / FAIL report. It only reads. A FAIL blocks the migration: the command exits with code 1, and `gophant snapshot` runs the same checks first and stops. Code: [internal/precheck](../internal/precheck). Each rule has a part that reads the facts (`checker.go`) and a pure part that judges them (`rules.go`), which is what the unit tests cover.

### Rules

| Rule | FAIL when | WARN when |
|---|---|---|
| `source.version` | s1 is older than PostgreSQL 13 | |
| `target.version` | s3 is older than s1 | s3 is not 18 |
| `source.wal_level` | `wal_level` is not `logical` | |
| `source.replication_role` | the role has neither `REPLICATION` nor superuser | |
| `source.read_privilege` | the role cannot `SELECT` a table in scope | |
| `source.sequence_privilege` | the role cannot read a sequence in scope (`pg_sequences.last_value` would be NULL and the cutover would skip it) | |
| `source.max_slot_wal_keep_size` | the value is not greater than 0 | it is not, but `precheck.allow_unlimited_slot_wal` is set |
| `source.replication_capacity` | no free replication slot or walsender | |
| `source.publication` | it does not exist, `publish_via_partition_root` is off, one of insert/update/delete/truncate is missing, or a table in scope is not covered | |
| `source.slot` | the slot exists but this migration does not track it, is invalidated, or uses another plugin; or the slot vanished after the snapshot | |
| `source.scope` | the filter selects no table | a schema of `filter.include_schemas` does not exist |
| `source.replica_identity` | UPDATE/DELETE cannot be replicated (no primary key with the default identity, `NOTHING`, `USING INDEX` without index) | `FULL` without a primary key |
| `source.unlogged_tables` | an UNLOGGED partitioned table (PostgreSQL 18 rejects it) | UNLOGGED tables: logical decoding does not see their changes |
| `source.generated_columns` | | lists them: they are left out of COPY and replay |
| `source.copy_format` | | lists the tables copied in text instead of binary format |
| `source.monitoring` | | the role cannot see other sessions, so the cutover cannot detect open write transactions |
| `source.large_objects` | | large objects exist; they are not migrated |
| `source.long_transactions` | | write transactions older than 5 minutes delay the creation of the slot |
| `source.md5_passwords` | | roles with an md5 hash, or `pg_authid` cannot be read |
| `target.extensions` | an extension of s1 is not available on s3, or lives outside `filter.include_schemas` (pg_dump would not create it), or `skip_ddl` is set and it is not installed | the installed version differs |
| `target.roles` | a role that owns or is granted an object in scope does not exist on s3 | |
| `target.state` | s3 already contains a table in scope (`skip_ddl = false`); or, with `skip_ddl = true`, lacks one or holds rows in one (the copy adds to what is there, so rows restored from a backup would be duplicated); skipped once the migration state exists | |
| `target.session_role` | the role cannot `SET session_replication_role` | |
| `target.encoding` | | encoding or collation differ |
| `tools.pg_dump`, `tools.pg_restore` | not runnable, or older than s1 | not version 18 |

Run `precheck` before creating the publication. The publication is the point after which UPDATE and DELETE on a table without usable replica identity fail on s1; the hint of `source.replica_identity` says how to fix each table.

### Lessons learned

* SQL does not promise the order in which `WHERE` conditions are evaluated. `has_sequence_privilege(oid, ...)` raised an error for TOAST tables although the query said `relkind = 'S'`. A `CASE WHEN relkind = 'S' THEN ... END` keeps the order.
* The role of the tool does not need to be a superuser: `pg_read_all_data` is enough to read the catalogs the checks use, `pg_read_all_stats` is only needed to see other sessions.
* A rule that cannot run (privileges, a failing query) becomes a FAIL entry with the error, it never silently disappears from the report.

---

# 中文

## Precheck

`gophant precheck` 检查 s1 和 s3 并输出 PASS / WARN / FAIL 报告，只读。FAIL 会阻断迁移：命令以退出码 1 结束，`gophant snapshot` 也会先运行同样的检查并停止。代码在 [internal/precheck](../internal/precheck)。每条规则分为读取事实的部分（`checker.go`）和判断事实的纯函数部分（`rules.go`），单元测试覆盖的是后者。

### 规则

| 规则 | 出现 FAIL 的情况 | 出现 WARN 的情况 |
|---|---|---|
| `source.version` | s1 低于 PostgreSQL 13 | |
| `target.version` | s3 版本低于 s1 | s3 不是 18 |
| `source.wal_level` | `wal_level` 不是 `logical` | |
| `source.replication_role` | 角色既没有 `REPLICATION` 也不是超级用户 | |
| `source.read_privilege` | 角色无法 `SELECT` 范围内的某张表 | |
| `source.sequence_privilege` | 角色无法读取范围内的某个序列（`pg_sequences.last_value` 会是 NULL，割接会跳过它） | |
| `source.max_slot_wal_keep_size` | 值不大于 0 | 值不大于 0，但设置了 `precheck.allow_unlimited_slot_wal` |
| `source.replication_capacity` | 没有空闲的复制槽或 walsender | |
| `source.publication` | 不存在、`publish_via_partition_root` 关闭、缺少 insert/update/delete/truncate 之一、或范围内有表未被覆盖 | |
| `source.slot` | 槽存在但不是本次迁移跟踪的、已失效或使用其他插件；或快照之后槽消失了 | |
| `source.scope` | filter 没有选中任何表 | `filter.include_schemas` 中有 schema 不存在 |
| `source.replica_identity` | UPDATE/DELETE 无法复制（默认 identity 且无主键、`NOTHING`、`USING INDEX` 但没有索引） | 无主键的 `FULL` |
| `source.unlogged_tables` | UNLOGGED 分区表（PostgreSQL 18 拒绝） | UNLOGGED 表：逻辑解码看不到它们的变更 |
| `source.generated_columns` | | 列出这些列：它们不参与 COPY 和回放 |
| `source.copy_format` | | 列出用文本而不是二进制格式拷贝的表 |
| `source.monitoring` | | 角色看不到其他会话，割接无法检测打开的写事务 |
| `source.large_objects` | | 存在大对象；它们不会被迁移 |
| `source.long_transactions` | | 超过 5 分钟的写事务会延迟槽的创建 |
| `source.md5_passwords` | | 仍使用 md5 哈希的角色，或无法读取 `pg_authid` |
| `target.extensions` | s1 的扩展在 s3 上不可用、位于 `filter.include_schemas` 之外（pg_dump 不会创建它）、或设置了 `skip_ddl` 而它没有安装 | 已安装的版本不同 |
| `target.roles` | 在范围内拥有对象或被授权的角色在 s3 上不存在 | |
| `target.state` | s3 已经包含范围内的表（`skip_ddl = false`）；或在 `skip_ddl = true` 时缺少表、或表里已有数据（拷贝是在已有内容之上追加，从备份恢复出来的行会被重复）；迁移状态存在之后跳过 | |
| `target.session_role` | 角色无法 `SET session_replication_role` | |
| `target.encoding` | | 编码或排序规则不同 |
| `tools.pg_dump`、`tools.pg_restore` | 无法运行，或版本低于 s1 | 不是 18 版本 |

请在创建 publication 之前运行 `precheck`。publication 是一个分界点：此后，s1 上对没有可用 replica identity 的表的 UPDATE 和 DELETE 就会失败；`source.replica_identity` 的 hint 说明了每张表怎么修。

### 经验教训

* SQL 不保证 `WHERE` 条件的求值顺序。查询里写了 `relkind = 'S'`，`has_sequence_privilege(oid, ...)` 仍然对 TOAST 表报错。用 `CASE WHEN relkind = 'S' THEN ... END` 才能保证顺序。
* 工具使用的角色不必是超级用户：`pg_read_all_data` 足以读取检查所用的目录表，只有查看其他会话才需要 `pg_read_all_stats`。
* 无法运行的规则（权限不足、查询失败）会变成带错误信息的 FAIL 条目，不会悄悄从报告里消失。
