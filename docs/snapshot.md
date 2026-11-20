# English

## Snapshot

`gophant snapshot` runs stages 1b and 2: it imports the table definitions, creates the replication slot with an exported snapshot, copies every table in parallel and imports indexes and constraints. Code: [internal/snapshot](../internal/snapshot) and [internal/schema](../internal/schema).

### Flow

1. Read the migration state from s3 and decide: fresh start, `--restart`, or continue a finished copy whose post-data import failed. An unfinished or finished snapshot without `--restart` is refused.
2. Run the precheck; any FAIL stops here.
3. Create the meta schema and the state row; read the plan from s1 (tables, columns, column types).
4. `pg_dump --schema-only --format=custom` into a temporary archive, then `pg_restore --section=pre-data` in one transaction. A `CREATE SCHEMA` of a schema that s3 already has is removed from the restore list (the archive of `--schema=public` contains `CREATE SCHEMA public`). Skipped with `snapshot.skip_ddl`.
5. Register every table in `table_progress` as `PENDING`.
6. Create the slot: `CREATE_REPLICATION_SLOT <slot> LOGICAL pgoutput EXPORT_SNAPSHOT` on a fresh replication connection. The result is `consistent_lsn` and the snapshot name; both are stored (stage `SNAPSHOT`). The command can wait for running transactions to finish and logs that every 30 seconds.
7. The workers copy the tables, largest first. For each table: on s1 `BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY; SET TRANSACTION SNAPSHOT '<name>'`, on s3 `BEGIN` with `session_replication_role = replica`, then `COPY ... TO STDOUT` is piped through `io.Pipe` into `COPY ... FROM STDIN`, without touching the disk. The s3 transaction commits only when the whole table arrived. Generated columns are left out of the column list.
8. When all tables are `DONE`, the slot connection is closed (the snapshot is released) and `pg_restore --section=post-data` runs with `snapshot.workers` jobs.
9. The stage becomes `CDC_STREAM`.

### The exported snapshot

* The snapshot is the state of s1 at `consistent_lsn`. Rows copied under it plus the WAL stream from `consistent_lsn` reproduce s1 without gap or overlap.
* It lives only while the replication connection that created it stays open **and idle**. Any command sent on it ends the snapshot. The connection is therefore kept untouched until the copy is over, and every table, and every retry of a table, imports the same snapshot name.
* Consequence: the snapshot does not survive the process. A crash or a failed table cannot be repaired table by table. `snapshot --restart` starts over.
* A long transaction on s1 holds back `VACUUM` for the whole copy, as for any consistent copy.

### Choices that matter

* **Copy unit.** Every ordinary table is copied, including each leaf partition, directly into the partition of the same name; a partitioned parent has no rows and is skipped. Tables owned by an extension are skipped (`CREATE EXTENSION` creates their rows on s3).
* **Binary or text.** `FORMAT BINARY` is used unless a column type is not portable between two databases: an array of a user-defined type and a composite type with user-defined fields carry type OIDs that differ between s1 and s3, and a type may have no binary I/O. Such tables use `FORMAT TEXT`. Plain enums are fine in binary.
* **Retries.** A table is tried three times for transient errors (connection loss, deadlock, shutdown). A retry truncates the table first: the previous attempt may have committed although the client saw an error, and without primary keys on s3 yet a second load would duplicate the rows silently.
* **Failure.** The first failure cancels the other workers. The failed table is `FAILED` with the error in `table_progress`, the others that were running go back to `PENDING`.
* **Progress.** The row counter of a running table is read from `pg_stat_progress_copy` of the s3 backend every 2 seconds, so `gophant status` shows it without any cost on the data path.

### Restart and resume

| Situation | Command | Effect |
|---|---|---|
| The copy failed or the process died | `snapshot --restart` | Asks for confirmation (`--yes`), drops the slot, truncates the tables of `table_progress`, resets the state. If every table is still on s3 the pre-data import is skipped. Post-data objects that exist are kept. |
| The copy finished, post-data failed | `snapshot` | Dumps the schema again (the DDL is frozen) and creates only the post-data objects that are missing. No table is copied again. |
| The snapshot is complete | `snapshot --restart` | Same as the first row; the migration starts over. |

Existing post-data objects are skipped by removing their entries from the `pg_restore` list (indexes, constraints, foreign keys, triggers). Dropping and recreating does not work: an inherited primary key of a partition cannot be dropped on its own.

### Lessons learned

* `pg_restore --exit-on-error` of the pre-data section failed with `schema "public" already exists` because the archive of an explicit `--schema=public` contains `CREATE SCHEMA public`. The restore list (`pg_restore --list`, then `--use-list`) is the supported way to leave one entry out.
* `pg_restore --clean --if-exists` failed with `cannot drop inherited constraint "<partition>_pkey"` on a partitioned table. Skipping what exists is the robust way to repeat post-data.
* `pg_dump` and `pg_restore` receive the connection string without password and the password in `PGPASSWORD`, so it never appears in the process list or in an error message.
* A table written during the copy is not a problem: its changes after `consistent_lsn` come through the WAL stream. This is covered by the end-to-end test, which writes to s1 during the snapshot.

---

# 中文

## Snapshot

`gophant snapshot` 执行阶段 1b 和 2：导入表结构，用导出快照创建复制槽，并发拷贝每张表，导入索引和约束。代码在 [internal/snapshot](../internal/snapshot) 和 [internal/schema](../internal/schema)。

### 流程

1. 从 s3 读取迁移状态并决定：全新开始、`--restart`，或继续一个已拷贝完成但 post-data 导入失败的快照。没有 `--restart` 时，未完成或已完成的快照会被拒绝。
2. 运行 precheck；有 FAIL 就在这里停止。
3. 创建 meta schema 和状态行；从 s1 读取计划（表、列、列类型）。
4. `pg_dump --schema-only --format=custom` 导出到临时归档，再用 `pg_restore --section=pre-data` 在一个事务中恢复。s3 已有的 schema 的 `CREATE SCHEMA` 会从恢复列表中去掉（显式 `--schema=public` 的归档包含 `CREATE SCHEMA public`）。设置 `snapshot.skip_ddl` 时跳过。
5. 把每张表登记到 `table_progress`，状态为 `PENDING`。
6. 创建槽：在新的复制连接上执行 `CREATE_REPLICATION_SLOT <slot> LOGICAL pgoutput EXPORT_SNAPSHOT`。结果是 `consistent_lsn` 和快照名，两者都会保存（阶段 `SNAPSHOT`）。该命令可能要等运行中的事务结束，等待期间每 30 秒记录一次日志。
7. worker 按从大到小的顺序拷贝表。对每张表：在 s1 上 `BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY; SET TRANSACTION SNAPSHOT '<name>'`，在 s3 上 `BEGIN`（会话为 `session_replication_role = replica`），然后 `COPY ... TO STDOUT` 经 `io.Pipe` 直接喂给 `COPY ... FROM STDIN`，不落盘。只有整张表都到达，s3 事务才提交。生成列不在列清单里。
8. 所有表都是 `DONE` 之后，关闭槽连接（释放快照），并以 `snapshot.workers` 个 job 运行 `pg_restore --section=post-data`。
9. 阶段变为 `CDC_STREAM`。

### 导出的快照

* 快照就是 s1 在 `consistent_lsn` 时的状态。在它之下拷贝的行，加上从 `consistent_lsn` 开始的 WAL 流，能不丢不重地还原 s1。
* 它只在创建它的复制连接保持打开**并且空闲**期间有效。在该连接上发送任何命令都会结束快照。因此在拷贝结束前该连接保持不动，每张表、每次表重试都导入同一个快照名。
* 结果：快照活不过进程。崩溃或某张表失败时无法按表修复，只能 `snapshot --restart` 重来。
* s1 上的长事务会在整个拷贝期间阻止 `VACUUM`，任何一致性拷贝都是如此。

### 重要的选择

* **拷贝单位。** 每个普通表都会拷贝，包括每个叶子分区，直接拷到同名分区；分区父表没有行，被跳过。属于扩展的表被跳过（它们的行由 `CREATE EXTENSION` 在 s3 上创建）。
* **二进制或文本。** 使用 `FORMAT BINARY`，除非某列类型在两个数据库之间不可移植：用户自定义类型的数组、含用户自定义字段的复合类型带有的类型 OID 在 s1 和 s3 上不同，有的类型还没有二进制 I/O。这样的表使用 `FORMAT TEXT`。普通 enum 用二进制没问题。
* **重试。** 遇到暂时性错误（断连、死锁、服务器关闭）时，一张表最多尝试三次。重试会先清空该表：上一次尝试可能已经提交，只是客户端看到了错误；而 s3 此时还没有主键，第二次加载会悄悄产生重复行。
* **失败。** 第一次失败会取消其他 worker。失败的表在 `table_progress` 中是 `FAILED` 并带有错误信息，其他正在运行的表回到 `PENDING`。
* **进度。** 运行中的表的行计数每 2 秒从 s3 后端的 `pg_stat_progress_copy` 读取，所以 `gophant status` 能显示它，而数据通路上没有任何开销。

### 重启与续跑

| 情况 | 命令 | 效果 |
|---|---|---|
| 拷贝失败或进程死亡 | `snapshot --restart` | 请求确认（`--yes`），删除槽，清空 `table_progress` 中的表，重置状态。如果所有表仍在 s3 上，则跳过 pre-data 导入。已存在的 post-data 对象保留。 |
| 拷贝完成，post-data 失败 | `snapshot` | 再次导出表结构（DDL 已冻结），只创建缺失的 post-data 对象。不会再拷贝任何表。 |
| 快照已完成 | `snapshot --restart` | 与第一行相同；迁移从头开始。 |

已存在的 post-data 对象通过从 `pg_restore` 列表里去掉它们的条目来跳过（索引、约束、外键、触发器）。先删后建行不通：分区继承来的主键无法单独删除。

### 经验教训

* pre-data 的 `pg_restore --exit-on-error` 因为 `schema "public" already exists` 失败，原因是显式 `--schema=public` 的归档包含 `CREATE SCHEMA public`。用恢复列表（`pg_restore --list`，再 `--use-list`）是去掉某个条目的受支持方式。
* `pg_restore --clean --if-exists` 在分区表上因 `cannot drop inherited constraint "<partition>_pkey"` 失败。跳过已存在的对象才是重复 post-data 的稳妥方式。
* `pg_dump` 和 `pg_restore` 收到的是不含密码的连接串，密码放在 `PGPASSWORD`，所以它不会出现在进程列表或错误信息里。
* 拷贝期间被写入的表不是问题：`consistent_lsn` 之后的变更会通过 WAL 流到达。端到端测试会在快照期间向 s1 写入，覆盖了这一点。
