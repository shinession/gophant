# English

## Operations guide

gophant migrates a PostgreSQL 16 database (s1) to PostgreSQL 18 (s3). It copies a consistent snapshot, replays the logical WAL stream exactly once, and finishes with a short cutover. This guide is the runbook. The design is in [gophant.md](gophant.md); the differences between the design and the code are in [design_deviations.md](design_deviations.md).

### 1. Prerequisites

On s1 (PostgreSQL 16), with a DBA:

1. `postgresql.conf`, needs one restart: `wal_level = logical`, `max_replication_slots = 8`, `max_wal_senders = 8`.
2. `postgresql.conf`, reload is enough: `max_slot_wal_keep_size = '30GB'` (size it to 30-50% of the free disk; without it a stalled slot can fill the disk of s1) and `wal_sender_timeout = '5min'`.
3. A dedicated role, and `pg_hba.conf` lines for both the database and `replication`.
4. Create the publication only after `gophant precheck` shows no failure for `source.replica_identity`: as soon as a publication covers a table without usable replica identity, UPDATE and DELETE on that table fail on s1.
5. Freeze the DDL of s1 from `snapshot` until the end of the cutover.

```sql
CREATE ROLE gophant_repl WITH LOGIN REPLICATION PASSWORD '<strong password>';
GRANT pg_read_all_data TO gophant_repl;
GRANT pg_read_all_stats TO gophant_repl;  -- lets the cutover see open write transactions
CREATE PUBLICATION gophant_pub FOR ALL TABLES WITH (publish_via_partition_root = true);
```

```text
host    appdb           gophant_repl    <gophant_ip>/32    scram-sha-256
host    replication     gophant_repl    <gophant_ip>/32    scram-sha-256
```

On s3 (PostgreSQL 18): an empty database with the same encoding and collation, the same roles that own or are granted objects on s1 (`pg_dumpall --roles-only`), every extension of s1 available, and a role that is a superuser or may `SET session_replication_role`. The PostgreSQL 18 `pg_dump` and `pg_restore` must be installed where gophant runs; `tools.pg_dump_bin` and `tools.pg_restore_bin` point to them.

### 2. Run it

Passwords belong in environment variables: any `${NAME}` in a string value of the configuration is replaced by that variable, and an unset variable is an error. Start from [configs/gophant.example.yaml](../configs/gophant.example.yaml).

| Step | Command | What happens |
|---|---|---|
| 1 | `gophant precheck -c gophant.yaml` | Read-only checks, a PASS / WARN / FAIL report, exit code 1 on any FAIL. |
| 2 | `gophant snapshot -c gophant.yaml` | Runs the precheck, imports pre-data, creates the slot with an exported snapshot, copies all tables in parallel, imports post-data. Stage becomes `CDC_STREAM`. |
| 3 | `gophant sync -c gophant.yaml` | Replays the WAL stream until stopped. Keep it running; stop it with Ctrl-C or SIGTERM. It resumes from `applied_lsn` after a restart and reconnects by itself. |
| 4 | Stop all writes to s1 | Stop the application or set the database read-only. gophant does not do this for you. |
| 5 | `gophant cutover -c gophant.yaml` | Asks for confirmation (`--yes` skips it), waits for lag zero, synchronizes all sequences, sets the stage to `VERIFY`. `sync` stops by itself. |
| 6 | `gophant verify -c gophant.yaml` | Row counts and primary key chunk hashes. Exit code 1 on any difference. |
| 7 | Switch the application to s3 | |
| 8 | `gophant verify --drop-slot -c gophant.yaml` | Drops the slot on s1, which releases the WAL it keeps, and sets the stage to `DONE`. |

`gophant status -c gophant.yaml [--json]` shows the stage, the lag in bytes, the slot and the progress of every table at any time.

Global flags: `-c/--config` (default `gophant.yaml`), `--log-level debug|info|warn|error`, `--log-format json|text`. Reports go to stdout, logs to stderr.

### 3. Stages

| Stage | Set by | Meaning |
|---|---|---|
| `PRECHECK` | `snapshot` | The state row exists, the slot is not created yet. |
| `SNAPSHOT` | `snapshot` | The slot exists and the tables are being copied. |
| `CDC_STREAM` | `snapshot` | Copy and post-data are done; `sync` replays the stream. |
| `CUTOVER` | `cutover` | Waiting for lag zero. |
| `VERIFY` | `cutover` | s3 has everything; `sync` has stopped; verification may run. |
| `DONE` | `verify --drop-slot` | The slot is dropped. |

### 4. Configuration reference

| Key | Default | Meaning |
|---|---|---|
| `source.dsn` | required | SQL connection to s1. |
| `source.replication_dsn` | `source.dsn` | Replication connection (`replication=database`). |
| `source.slot_name` | `gophant_slot` | Lower-case letters, digits, underscores. |
| `source.publication_name` | `gophant_pub` | Must exist on s1. |
| `target.dsn` | required | Connection to s3. |
| `target.meta_schema` | `gophant_meta` | Schema of the state tables on s3. |
| `tools.pg_dump_bin`, `tools.pg_restore_bin` | `pg_dump`, `pg_restore` | PostgreSQL 18 client tools. |
| `filter.include_schemas` | all non-system schemas | Exact schema names. |
| `filter.exclude_tables` | none | Schema-qualified patterns, for example `public.audit_*`. |
| `precheck.allow_unlimited_slot_wal` | `false` | Turns the `max_slot_wal_keep_size` FAIL into a WARN. |
| `snapshot.workers` | 4 | Tables copied at the same time; also the `--jobs` of the post-data import. |
| `snapshot.skip_ddl` | `false` | Skip `pg_dump`/`pg_restore`; the schema on s3 is prepared by hand. |
| `cdc.standby_status_interval` | `5s` | How often the position is reported to s1 (1 s during the cutover). |
| `cdc.batch_max_txns`, `cdc.batch_max_delay` | 50, `200ms` | A micro-batch merges up to this many source transactions or this much time into one s3 transaction. |
| `cdc.max_tx_memory_mb` | 64 | A transaction above this size is spilled to `cdc.temp_dir`. |
| `cdc.temp_dir` | `<system temp>/gophant-spill` | Spill directory. |
| `cutover.sequence_offset` | 100 | Safety gap added to every sequence. |
| `cutover.lag_zero_timeout` | `5m` | How long the cutover waits for lag zero. |
| `verify.workers`, `verify.chunk_size` | 4, 50000 | Parallelism, rows per primary key range. |
| `verify.mode` | `count_and_sample` | `count_only`, `count_and_sample` or `full_hash`. |
| `verify.sample_ratio` | 0.1 | Share of the chunks hashed in `count_and_sample`, at least one per table. |

### 5. Recovery

| Situation | What to do |
|---|---|
| `precheck` shows a FAIL | Fix it; the hint under the entry says how. |
| The copy failed | `gophant status` shows the table. Fix the cause, then `gophant snapshot --restart`. |
| The copy finished, the post-data import failed | Fix the cause, run `gophant snapshot` again: only the post-data import is repeated. |
| `sync` was stopped or killed | Start it again. It continues from `applied_lsn`; nothing is lost or applied twice. |
| `sync` lost the connection | It reconnects by itself (1 s growing to 30 s). A permanent error (for example a constraint violation or schema drift) stops it with the reason. |
| `sync` says the slot is beyond the position of s3 | s3 lost state (a restore, a reset of `gophant_meta`). The missing WAL is gone: `gophant snapshot --restart`. |
| `sync` says the slot is invalidated | `max_slot_wal_keep_size` was exceeded while `sync` was down. `gophant snapshot --restart`, and raise the limit or keep `sync` running. |
| The cutover timed out | The message gives the lag, the open write transactions and whether `sync` runs. The stage stays `CUTOVER`; fix the cause and run `cutover` again. |
| `verify` found a difference | Do not switch the application. The report names the table and the primary key range. |
| `--restart` | Drops the slot, truncates the copied tables, resets the state, copies again. It asks for confirmation; `--yes` skips the question. |

---

# 中文

## 运行手册

gophant 把 PostgreSQL 16 数据库（s1）迁移到 PostgreSQL 18（s3）：拷贝一致性快照，exactly-once 回放逻辑 WAL 流，最后用一个很短的割接收尾。本文是运行手册。设计见 [gophant.md](gophant.md)；设计与代码的差异见 [design_deviations.md](design_deviations.md)。

### 1. 前置条件

s1（PostgreSQL 16）上，由 DBA 操作：

1. `postgresql.conf`，需要重启一次：`wal_level = logical`、`max_replication_slots = 8`、`max_wal_senders = 8`。
2. `postgresql.conf`，reload 即可：`max_slot_wal_keep_size = '30GB'`（按 s1 剩余磁盘的 30%-50% 设置；不设置时，停滞的槽可能写满 s1 的磁盘）和 `wal_sender_timeout = '5min'`。
3. 专用角色，以及数据库和 `replication` 两类 `pg_hba.conf` 条目。
4. 等 `gophant precheck` 中 `source.replica_identity` 没有 FAIL 之后再创建 publication：只要 publication 覆盖了没有可用 replica identity 的表，s1 上对该表的 UPDATE 和 DELETE 就会失败。
5. 从 `snapshot` 开始到割接结束，冻结 s1 的 DDL。

SQL 和 `pg_hba.conf` 的内容与英文版相同，见上面的代码块。

s3（PostgreSQL 18）上：一个编码和排序规则相同的空数据库；在 s1 上拥有对象或被授权的角色必须已存在（`pg_dumpall --roles-only`）；s1 的所有扩展都可用；角色是超级用户，或被允许 `SET session_replication_role`。运行 gophant 的机器上必须装有 PostgreSQL 18 的 `pg_dump` 和 `pg_restore`，由 `tools.pg_dump_bin` 和 `tools.pg_restore_bin` 指向。

### 2. 执行

密码应放在环境变量里：配置文件中字符串值里的 `${NAME}` 会被替换为该变量，变量未设置则报错。从 [configs/gophant.example.yaml](../configs/gophant.example.yaml) 开始。

| 步骤 | 命令 | 发生什么 |
|---|---|---|
| 1 | `gophant precheck -c gophant.yaml` | 只读检查，输出 PASS / WARN / FAIL 报告，有 FAIL 时退出码为 1。 |
| 2 | `gophant snapshot -c gophant.yaml` | 先运行 precheck，导入 pre-data，用导出快照创建槽，并发拷贝所有表，导入 post-data。阶段变为 `CDC_STREAM`。 |
| 3 | `gophant sync -c gophant.yaml` | 持续回放 WAL 流直到被停止。保持运行；用 Ctrl-C 或 SIGTERM 停止。重启后从 `applied_lsn` 续跑，断线会自动重连。 |
| 4 | 停止对 s1 的所有写入 | 停应用，或把数据库设为只读。gophant 不会替你做。 |
| 5 | `gophant cutover -c gophant.yaml` | 请求确认（`--yes` 跳过），等待 lag 归零，同步所有序列，把阶段设为 `VERIFY`。`sync` 会自行停止。 |
| 6 | `gophant verify -c gophant.yaml` | 行数和主键分片哈希。有差异时退出码为 1。 |
| 7 | 把应用切换到 s3 | |
| 8 | `gophant verify --drop-slot -c gophant.yaml` | 删除 s1 上的槽，释放它保留的 WAL，并把阶段设为 `DONE`。 |

`gophant status -c gophant.yaml [--json]` 随时显示阶段、以字节计的 lag、槽以及每张表的进度。

全局参数：`-c/--config`（默认 `gophant.yaml`）、`--log-level debug|info|warn|error`、`--log-format json|text`。报告输出到 stdout，日志输出到 stderr。

### 3. 阶段

| 阶段 | 由谁设置 | 含义 |
|---|---|---|
| `PRECHECK` | `snapshot` | 状态行已存在，槽还没创建。 |
| `SNAPSHOT` | `snapshot` | 槽已存在，正在拷贝表。 |
| `CDC_STREAM` | `snapshot` | 拷贝和 post-data 已完成；`sync` 回放流。 |
| `CUTOVER` | `cutover` | 等待 lag 归零。 |
| `VERIFY` | `cutover` | s3 已拥有全部数据；`sync` 已停止；可以校验。 |
| `DONE` | `verify --drop-slot` | 槽已删除。 |

### 4. 配置参考

| 配置项 | 默认值 | 含义 |
|---|---|---|
| `source.dsn` | 必填 | 连接 s1 的 SQL 连接。 |
| `source.replication_dsn` | `source.dsn` | 复制连接（`replication=database`）。 |
| `source.slot_name` | `gophant_slot` | 小写字母、数字、下划线。 |
| `source.publication_name` | `gophant_pub` | 必须已存在于 s1。 |
| `target.dsn` | 必填 | 连接 s3。 |
| `target.meta_schema` | `gophant_meta` | s3 上状态表所在的 schema。 |
| `tools.pg_dump_bin`、`tools.pg_restore_bin` | `pg_dump`、`pg_restore` | PostgreSQL 18 客户端工具。 |
| `filter.include_schemas` | 所有非系统 schema | 精确的 schema 名。 |
| `filter.exclude_tables` | 无 | 带 schema 的模式，例如 `public.audit_*`。 |
| `precheck.allow_unlimited_slot_wal` | `false` | 把 `max_slot_wal_keep_size` 的 FAIL 降为 WARN。 |
| `snapshot.workers` | 4 | 同时拷贝的表数；也是 post-data 导入的 `--jobs`。 |
| `snapshot.skip_ddl` | `false` | 跳过 `pg_dump`/`pg_restore`；s3 的表结构由人工准备。 |
| `cdc.standby_status_interval` | `5s` | 向 s1 汇报位点的间隔（割接期间为 1 秒）。 |
| `cdc.batch_max_txns`、`cdc.batch_max_delay` | 50、`200ms` | 一个微批最多把这么多源端事务、或这么长时间内的事务合并进一个 s3 事务。 |
| `cdc.max_tx_memory_mb` | 64 | 超过该大小的事务溢写到 `cdc.temp_dir`。 |
| `cdc.temp_dir` | `<系统临时目录>/gophant-spill` | 溢写目录。 |
| `cutover.sequence_offset` | 100 | 加到每个序列上的安全间隔。 |
| `cutover.lag_zero_timeout` | `5m` | 割接等待 lag 归零的时间。 |
| `verify.workers`、`verify.chunk_size` | 4、50000 | 并发度、每个主键区间的行数。 |
| `verify.mode` | `count_and_sample` | `count_only`、`count_and_sample` 或 `full_hash`。 |
| `verify.sample_ratio` | 0.1 | `count_and_sample` 下计算哈希的分片比例，每张表至少一个。 |

### 5. 故障恢复

| 情况 | 处理方式 |
|---|---|
| `precheck` 出现 FAIL | 修复它；条目下方的 hint 说明如何修复。 |
| 拷贝失败 | `gophant status` 会显示是哪张表。修复原因后执行 `gophant snapshot --restart`。 |
| 拷贝完成但 post-data 导入失败 | 修复原因后再次运行 `gophant snapshot`：只会重复 post-data 导入。 |
| `sync` 被停止或被 kill | 再次启动即可。它从 `applied_lsn` 继续，不丢也不会重复回放。 |
| `sync` 断线 | 它会自行重连（间隔从 1 秒增长到 30 秒）。永久性错误（例如约束冲突、schema drift）会让它带着原因停止。 |
| `sync` 提示槽已超过 s3 的位点 | s3 丢失了状态（恢复备份、重置了 `gophant_meta`）。缺失的 WAL 已不存在：执行 `gophant snapshot --restart`。 |
| `sync` 提示槽已失效 | `sync` 停止期间超过了 `max_slot_wal_keep_size`。执行 `gophant snapshot --restart`，并提高限制或让 `sync` 保持运行。 |
| 割接超时 | 信息中给出 lag、打开的写事务数以及 `sync` 是否在运行。阶段保持 `CUTOVER`；修复原因后再次运行 `cutover`。 |
| `verify` 发现差异 | 不要切换应用。报告会指出表和主键区间。 |
| `--restart` | 删除槽、清空已拷贝的表、重置状态、重新拷贝。它会请求确认；`--yes` 跳过询问。 |
