# English

## Deviations from the design specification

[gophant.md](gophant.md) is the specification the implementation follows. While building it against real PostgreSQL 16 and 18 servers, some statements turned out to be impossible, incomplete or risky when followed to the letter. This document lists every place where the code does something different from, or more precise than, the specification. Everything here describes the current implementation.

### A. Corrections: the literal text cannot work

| # | Specification | Implementation | Why |
|---|---|---|---|
| A1 | `CREATE_REPLICATION_SLOT ... EXPORT_SNAPSHOT` is run "inside a transaction block" (section 6.2). | It is the first and only command of a fresh replication connection, outside any transaction. The connection then stays open and idle until the copy is over. | PostgreSQL rejects `EXPORT_SNAPSHOT` inside a transaction block (`USE_SNAPSHOT` is the variant that needs one). The exported snapshot lives until a new command is sent on that connection, so nothing may be sent on it. |
| A2 | Lag zero means `pg_current_wal_lsn() == applied_lsn` (sections 2.2 and 6.5). | The applier moves `applied_lsn` forward on an idle stream (see B3), and `cutover` accepts lag zero when `applied_lsn` has reached the WAL position of one poll earlier, with no open write transaction and no prepared transaction, for three polls in a row. | After the last transaction s1 still writes records of its own (checkpoints, autovacuum), so the position moves under the poll. A strict equality at one instant is not reliable. |
| A3 | A transaction is delivered only when `CommitLSN > applied_lsn` (section 6.3). | `applied_lsn` holds the end position of the last applied transaction (`TransactionEndLSN`), and a transaction is delivered when `CommitLSN >= applied_lsn`. | The next commit record can start exactly where the previous one ended, so `>` would skip a transaction. The same holds for the first transaction after `consistent_lsn`. With end positions and `>=` there is no loss and no duplicate. |
| A4 | Tables without a primary key are a "WARN or block" (section 6.1). | A table whose UPDATE/DELETE cannot be replicated (no primary key with the default replica identity, `REPLICA IDENTITY NOTHING`, or `USING INDEX` without an index) is a FAIL. `REPLICA IDENTITY FULL` without a primary key is a WARN. | Once a publication covers such a table, UPDATE and DELETE on it fail on s1 itself. That is a production outage, not a note. |
| A5 | The replay writes every published column. | `INSERT` into a `GENERATED ALWAYS AS IDENTITY` column uses `OVERRIDING SYSTEM VALUE`, and such a column is never assigned by `UPDATE`. | Without it the first insert into any table with an identity-always column fails on s3. |

### B. Additions: behaviour the specification does not describe

| # | Topic | Implementation |
|---|---|---|
| B1 | Snapshot failure | One exported snapshot cannot outlive its process, so an interrupted copy cannot be resumed table by table. `snapshot --restart` discards it (drops the slot, truncates the copied tables, resets `gophant_meta`) and starts over. A copy that finished while the post-data import failed is continued by running `snapshot` again: it only repeats the post-data import. |
| B2 | Partitions | The unit of copy is every ordinary table, including each leaf partition. A partitioned parent holds no rows and is not copied; its partitions are copied in parallel. Tables that belong to an extension are not copied. The replay uses the root table name (`publish_via_partition_root`). |
| B3 | Idle position | The reader hands the position of a caught-up, idle stream (the `ServerWALEnd` of a keepalive, with nothing open and nothing in flight) to the applier, which stores it as `applied_lsn`. The heartbeat asks for a reply every time, so an idle stream keeps moving. |
| B4 | Binary COPY | A table with a column type that is not portable in binary COPY between two databases (an array of a user-defined type, a composite type with user-defined fields, a type without binary I/O) is copied with `FORMAT TEXT`. The `source.copy_format` check lists these tables. |
| B5 | Schema import | One `pg_dump --schema-only --format=custom` archive serves both steps (`--no-publications --no-subscriptions --no-tablespaces`). Pre-data is restored in one transaction, post-data with `--jobs`. A `CREATE SCHEMA` for a schema that s3 already has (`public`) is removed from the restore list. After a restart or a failed attempt, post-data objects that already exist are skipped instead of being dropped and recreated (an inherited partition constraint cannot be dropped alone). |
| B6 | Replication connection | `client_encoding=UTF8`, `DateStyle=ISO`, `IntervalStyle=postgres` and `extra_float_digits=3` are forced, the way the built-in subscriber does, so that a cluster default on s1 cannot change how values are rendered. All connections also switch `statement_timeout`, `lock_timeout` and `idle_in_transaction_session_timeout` off. |
| B7 | Exactly-once safeguard | The statement that moves `applied_lsn` only matches when the stored position is behind the batch. If it matches no row, the replay transaction is rolled back: the batch would have been applied a second time. |
| B8 | Result checks | An UPDATE that does not find exactly one row is an error (the databases diverged). A DELETE that finds no row is logged as a warning and counted, because the wanted end state holds. |
| B9 | No primary key | Rows are located by the full old row, `NULL` with `IS NULL`. The `ctid` guard from the specification is used, with `ONLY`; for a partitioned root it is `(tableoid, ctid)`. Columns of a type without a usable `=` (json, xml, geometric types) are compared by their text form. |
| B10 | Sequences | The offset moves in the direction of the sequence (a descending sequence is decreased) and the result is clamped to the bounds of the sequence. A sequence that does not exist on s3 is reported and skipped. |
| B11 | Verification | The fingerprint of a primary key range is the row count and the **sum** of 64-bit hashes of the whole-row text, not an XOR: an XOR cancels two identical rows. Both servers run the same query with the same `DateStyle`, `TimeZone`, `IntervalStyle`, `extra_float_digits` and `bytea_output`. A table without a primary key is hashed as one chunk only in `full_hash` mode. |
| B12 | Cutover | gophant does not stop the writes to s1; the operator does, and confirms (or passes `--yes`). The cutover refuses to finish while another session holds a write transaction or a prepared transaction is open (needs `pg_read_all_stats` or a superuser, otherwise it warns), and while no `gophant sync` consumes the slot. |
| B13 | Safety on start | `sync` refuses to start when the slot of s1 is already beyond the position stored on s3 (s3 lost state), when the slot is invalidated (`wal_status = lost`), or when it does not exist. |
| B14 | Memory | Transactions that wait for the applier are limited to about twice `cdc.max_tx_memory_mb`; a slow s3 slows the reader instead of exhausting memory. The heartbeat runs in its own goroutine, so a busy applier never causes a walsender timeout. |

### C. Additions to the project layout and the configuration

* New packages: `internal/catalog` (table, column and type queries), `internal/pg` (connections, DSN handling, error classes), `internal/format`, `internal/syncer` (the `sync` stage), `internal/cutover`, `internal/status`; `test/e2e` (Docker Compose test).
* New configuration: `precheck.allow_unlimited_slot_wal` (the specification says "unless the configuration explicitly allows it" without naming a key); `source.replication_dsn` is optional; string values may reference environment variables as `${NAME}`; `filter.exclude_tables` takes schema-qualified patterns (`schema.table`, `*` and `?`).
* New command flags: `snapshot --restart --yes`, `cutover --yes`, `verify --drop-slot --force`, `status --json`, and the global `--log-level` and `--log-format`.

### D. Known limits

* One table is copied by one stream. A single huge table that is not partitioned is not split.
* The DDL of s1 must stay frozen from `snapshot` to the end of the cutover. A column that exists on s1 but not on s3 stops the replay with a "schema drift" error instead of dropping data.
* Large objects, the contents of materialized views after the post-data import, and the data written to UNLOGGED tables after the copy are not migrated; `precheck` warns about the first and the third.
* Tested: PostgreSQL 16 to 18 in Docker, one small data set, the scenarios in [testing.md](testing.md). Not measured: throughput on large data, TLS connections, a PostgreSQL 16 server with a different encoding or collation.

---

# 中文

## 与设计方案的偏差

[gophant.md](gophant.md) 是实现所遵循的设计方案。在真实的 PostgreSQL 16 和 18 服务器上落地时，有些表述按字面执行会无法工作、不完整或有风险。本文列出代码中所有与设计方案不同或比它更精确的地方。这里描述的都是当前实现。

### A. 更正：字面文字无法工作

| # | 设计方案 | 实现 | 原因 |
|---|---|---|---|
| A1 | `CREATE_REPLICATION_SLOT ... EXPORT_SNAPSHOT` 在"事务块内"执行（6.2 节）。 | 它是新建复制连接上的第一条、也是唯一一条命令，不在任何事务内。之后连接保持打开并空闲，直到拷贝结束。 | PostgreSQL 拒绝在事务块内执行 `EXPORT_SNAPSHOT`（需要事务块的是 `USE_SNAPSHOT`）。导出的快照在该连接上发送新命令之前一直有效，所以连接上不能再发送任何命令。 |
| A2 | lag 归零即 `pg_current_wal_lsn() == applied_lsn`（2.2 和 6.5 节）。 | 回放器在流空闲时推进 `applied_lsn`（见 B3）；`cutover` 在 `applied_lsn` 达到上一次轮询时的 WAL 位置、没有打开的写事务和 prepared transaction、并连续三次轮询都满足时，认为 lag 归零。 | 最后一个事务之后 s1 仍会写自己的记录（checkpoint、autovacuum），位点在轮询期间不断移动，某一时刻的严格相等并不可靠。 |
| A3 | 仅当 `CommitLSN > applied_lsn` 时投递事务（6.3 节）。 | `applied_lsn` 保存最后一个已回放事务的结束位置（`TransactionEndLSN`），事务在 `CommitLSN >= applied_lsn` 时投递。 | 下一个提交记录可能恰好从上一个结束的位置开始，用 `>` 会漏掉事务，`consistent_lsn` 之后的第一个事务也一样。用结束位置加 `>=` 既不丢也不重。 |
| A4 | 无主键表是"WARN 或阻断"（6.1 节）。 | UPDATE/DELETE 无法复制的表（默认 replica identity 且无主键、`REPLICA IDENTITY NOTHING`、`USING INDEX` 但没有索引）是 FAIL；无主键的 `REPLICA IDENTITY FULL` 是 WARN。 | publication 一旦覆盖这样的表，s1 上对它的 UPDATE 和 DELETE 本身就会失败，这是生产故障，不只是提示。 |
| A5 | 回放写入所有已发布的列。 | 向 `GENERATED ALWAYS AS IDENTITY` 列 `INSERT` 时使用 `OVERRIDING SYSTEM VALUE`，`UPDATE` 从不为这类列赋值。 | 否则向任何带 identity-always 列的表的第一次插入就会在 s3 上失败。 |

### B. 补充：设计方案没有描述的行为

| # | 主题 | 实现 |
|---|---|---|
| B1 | 快照失败 | 一个导出的快照活不过它所在的进程，所以中断的拷贝不能按表续跑。`snapshot --restart` 丢弃它（删除槽、清空已拷贝的表、重置 `gophant_meta`）并从头开始。拷贝已完成但 post-data 导入失败时，再次运行 `snapshot` 即可继续，它只重复 post-data 导入。 |
| B2 | 分区表 | 拷贝单位是每个普通表，包括每个叶子分区。分区父表本身没有行，不拷贝；它的分区并发拷贝。属于扩展的表不拷贝。回放使用根表名（`publish_via_partition_root`）。 |
| B3 | 空闲位点 | 流已追平且空闲时（keepalive 的 `ServerWALEnd`，没有打开的事务也没有在途事务），reader 把该位点交给回放器，回放器将其保存为 `applied_lsn`。心跳每次都请求回复，所以空闲的流会持续前进。 |
| B4 | 二进制 COPY | 含有在两个数据库之间无法用二进制 COPY 移植的列类型（用户自定义类型的数组、含用户自定义字段的复合类型、没有二进制 I/O 的类型）的表，用 `FORMAT TEXT` 拷贝。`source.copy_format` 检查项会列出这些表。 |
| B5 | 表结构导入 | 一个 `pg_dump --schema-only --format=custom` 归档同时服务两个步骤（`--no-publications --no-subscriptions --no-tablespaces`）。pre-data 在单个事务中恢复，post-data 使用 `--jobs`。s3 已有的 schema（`public`）的 `CREATE SCHEMA` 会从恢复列表中去掉。重启或失败后重跑时，已存在的 post-data 对象被跳过，而不是先删后建（分区继承来的约束无法单独删除）。 |
| B6 | 复制连接 | 强制 `client_encoding=UTF8`、`DateStyle=ISO`、`IntervalStyle=postgres`、`extra_float_digits=3`，与内置订阅端一致，这样 s1 的集群默认值不会改变值的呈现方式。所有连接还会关闭 `statement_timeout`、`lock_timeout` 和 `idle_in_transaction_session_timeout`。 |
| B7 | exactly-once 保险 | 推进 `applied_lsn` 的语句只在已存储位点落后于该批次时才会命中。如果没有命中任何行，回放事务会回滚：这一批会被第二次应用。 |
| B8 | 结果检查 | UPDATE 找不到恰好一行是错误（两个库已经分叉）。DELETE 找不到行只记录警告并计数，因为想要的最终状态已经成立。 |
| B9 | 无主键 | 用完整的旧行定位，`NULL` 用 `IS NULL`。使用设计方案中的 `ctid` 保护，并带 `ONLY`；分区根表使用 `(tableoid, ctid)`。没有可用 `=` 的类型（json、xml、几何类型）的列按其文本形式比较。 |
| B10 | 序列 | 偏移量沿序列的方向移动（递减序列做减法），结果限制在序列的边界内。s3 上不存在的序列会被报告并跳过。 |
| B11 | 校验 | 主键区间的指纹是行数加上整行文本的 64 位哈希**之和**，而不是 XOR：XOR 会让两条相同的行相互抵消。两台服务器用相同的 `DateStyle`、`TimeZone`、`IntervalStyle`、`extra_float_digits`、`bytea_output` 执行同一条查询。无主键表只在 `full_hash` 模式下作为一个分片计算哈希。 |
| B12 | 割接 | gophant 不会停止对 s1 的写入，由操作员停止并确认（或传 `--yes`）。如果其他会话还持有写事务、或存在 prepared transaction，割接拒绝完成（需要 `pg_read_all_stats` 或超级用户，否则只给出警告）；没有 `gophant sync` 在消费槽时也会拒绝。 |
| B13 | 启动安全检查 | 当 s1 上的槽已经超过 s3 上存储的位点（s3 丢失了状态）、槽已失效（`wal_status = lost`）或槽不存在时，`sync` 拒绝启动。 |
| B14 | 内存 | 等待回放器的事务限制在约两倍的 `cdc.max_tx_memory_mb` 以内；s3 变慢时是 reader 变慢，而不是内存耗尽。心跳运行在独立的 goroutine 中，回放器再忙也不会造成 walsender 超时。 |

### C. 项目结构与配置的新增

* 新增 package：`internal/catalog`（表、列、类型查询）、`internal/pg`（连接、DSN 处理、错误分类）、`internal/format`、`internal/syncer`（`sync` 阶段）、`internal/cutover`、`internal/status`；`test/e2e`（Docker Compose 测试）。
* 新增配置：`precheck.allow_unlimited_slot_wal`（设计方案只写"除非配置显式允许"，没有给出配置项名）；`source.replication_dsn` 可省略；字符串值可用 `${NAME}` 引用环境变量；`filter.exclude_tables` 使用带 schema 的模式（`schema.table`，支持 `*` 和 `?`）。
* 新增命令参数：`snapshot --restart --yes`、`cutover --yes`、`verify --drop-slot --force`、`status --json`，以及全局的 `--log-level` 和 `--log-format`。

### D. 已知限制

* 一张表由一个流拷贝。没有分区的单个超大表不会被拆分。
* 从 `snapshot` 到割接结束，s1 的 DDL 必须保持冻结。s1 上有而 s3 上没有的列会让回放以"schema drift"错误停止，而不是丢弃数据。
* 大对象、post-data 导入之后物化视图的内容、拷贝之后写入 UNLOGGED 表的数据不会被迁移；`precheck` 会对第一项和第三项给出警告。
* 已测试：Docker 中 PostgreSQL 16 到 18，一份小数据集，场景见 [testing.md](testing.md)。未测量：大数据量下的吞吐、TLS 连接、编码或排序规则不同的 PostgreSQL 16 服务器。
