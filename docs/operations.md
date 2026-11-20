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

Size `max_slot_wal_keep_size` for the whole time until `sync` consumes the slot: the copy, **plus** the post-data import (the indexes are built on s3 before `sync` can start), plus the catch-up. s1 writes WAL for all of that time, and if more than the limit piles up the slot is invalidated (`wal_status = lost`) and the snapshot has to be restarted. Measure the rate of s1 before: read `SELECT wal_bytes FROM pg_stat_wal;` twice, an hour apart, during a busy period. Plan for at least 1.5 to 2 times the expected volume, and keep the limit below the free disk of s1, because the limit only caps what is kept: a disk smaller than the limit fills first. `wal_level = logical` itself writes somewhat more WAL than `replica`.

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

Global flags: `-c/--config` (default `gophant.yaml`), `-d/--database <name>` (only with `databases.auto`, repeatable, see section 7), `--log-level debug|info|warn|error`, `--log-format json|text`. Reports go to stdout, logs to stderr.

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
| `databases.auto` | `false` | Migrate every database of s1 instead of one, see section 7. |
| `databases.include`, `databases.exclude` | all found, none | Databases to migrate and never to migrate: names or patterns with `*` and `?`. |
| `databases.parallel` | 1 | Databases that are snapshotted (and verified) at the same time. |
| `databases.create_target`, `databases.create_publication` | `true`, `true` | Create a database that s3 lacks, and a publication that s1 lacks (needs a superuser on s1). |
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
| s3 was shut down, restarted or crashed | Start s3. A running `sync` retries by itself and continues; a stopped one is started again with `gophant sync`. Nothing is lost or applied twice. s1 keeps the WAL while s3 is down; the last question of section 6 says how long that is safe. |
| `sync` says the slot is beyond the position of s3 | s3 lost state (a restore, a reset of `gophant_meta`). The missing WAL is gone: `gophant snapshot --restart`. |
| `sync` says the slot is invalidated | `max_slot_wal_keep_size` was exceeded while `sync` was down. `gophant snapshot --restart`, and raise the limit or keep `sync` running. |
| The cutover timed out | The message gives the lag, the open write transactions and whether `sync` runs. The stage stays `CUTOVER`; fix the cause and run `cutover` again. |
| `verify` found a difference | Do not switch the application. The report names the table and the primary key range. |
| `--restart` | Drops the slot, truncates the copied tables, resets the state, copies again. It asks for confirmation; `--yes` skips the question. |

### 6. Common questions

**Can I restore a backup of s1 into s3 first and start the replication afterwards?** No, and it is not needed. The copy of `snapshot` is the full restore: it creates the slot, exports a snapshot and copies all tables under that snapshot, in parallel, while s1 stays online. Logical replication can only continue from the position of its slot, so a backup that was restored separately has no position to continue from: if the slot is created after the backup, the changes in between are lost; if it was created before, they arrive twice. With `snapshot.skip_ddl = true` the tables must exist **and be empty**; `precheck` fails on a table that holds rows. If you have to use your own restore procedure, the slot would have to be created first and the dump taken with `pg_dump --snapshot=<exported name>`; gophant has no mode for that yet.

**Where does gophant run, and does s3 have to be in Docker?** No. Docker is only the setup of the end-to-end tests. gophant is one binary without CGo. It runs on any host that can reach s1 (SQL and replication) and s3, and that has the PostgreSQL 18 `pg_dump` and `pg_restore` (set `tools.pg_dump_bin` and `tools.pg_restore_bin` to their paths). The copied rows stream through the gophant process, so the host should have a good link to both servers; the s3 machine itself is the natural choice, which leaves one network hop (s1 to gophant). On s3 allow the gophant host in `pg_hba.conf` and use a superuser (or a role that may `SET session_replication_role`, create extensions and change owners). TLS parameters of the connection strings are passed on to `pg_dump` and `pg_restore` but have not been tested.

**What does the long copy cost s1?** s1 stays writable. During the copy every worker holds a `REPEATABLE READ` transaction, so `VACUUM` cannot remove dead rows that appeared after the snapshot; tables with many updates grow until the copy ends. Every worker also reads from s1, so lower `snapshot.workers` if s1 is busy. The DDL of s1 must not change, and the slot holds back WAL (see section 1).

**Can s3 be shut down or restarted for a few hours once the snapshot is done? Must everything be copied again?** No. `applied_lsn` is committed on s3 in the same transaction as the rows, and s1 only moves the slot forward after s3 has committed, so a shutdown, a restart or a crash of s3 loses nothing and replays nothing twice: `sync` continues from `applied_lsn`. A `sync` that keeps running retries by itself (1 s growing to 30 s, a warning for every attempt) until s3 is back. A stopped one is started again with `gophant sync` once s3 accepts connections; started earlier, it exits at once with a connection error and changes nothing. Do not run `gophant snapshot` again: a finished snapshot is refused unless `--restart` is given.

The cost is on s1. While s3 is down nothing consumes the slot, so s1 keeps all WAL written since the last position that s3 committed, and `max_slot_wal_keep_size` is the limit for how long s3 may stay down: the room left divided by the WAL rate of s1 (section 1 shows how to measure it) is the number of hours. The query below shows the room left (empty when the limit is unlimited or the slot is lost). Before a long outage raise the limit on s1, a reload is enough, as far as the free disk of s1 allows.

```sql
SELECT slot_name, active, wal_status,
       pg_size_pretty(safe_wal_size) AS room_left,
       pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) AS retained
FROM pg_replication_slots WHERE slot_name = 'gophant_slot';
```

If the limit is exceeded the slot is invalidated (`wal_status = lost`), `sync` refuses to start and says to run `snapshot --restart`, which copies everything again. The copy is also needed again when s3 lost its progress: an s3 restored from an older backup or virtual machine snapshot, a deleted `gophant_meta`, or a database that was dropped and created again. `sync` notices a restored s3 (the slot is ahead of `applied_lsn`) and refuses to start instead of skipping changes. The slot that a recreated s3 leaves behind on s1 is a FAIL of `precheck`; drop it on s1 with `SELECT pg_drop_replication_slot('gophant_slot')` (or the name in `source.slot_name`).

While s3 is down or being set up, do not change the rows or the columns of the migrated tables on s3: `sync` expects every row of s1 to be on s3 exactly once and stops at the first one that does not fit (a duplicate key, an UPDATE that finds no row, a column that s3 lacks). Creating roles, installing extensions and changing server settings do not touch the replayed rows; the role of gophant on s3 must keep its login and its right to `SET session_replication_role`. Keep `fsync = on` on s3, which is the default. The applier makes every commit durable (`synchronous_commit = on`), but with `fsync = off`, sometimes set to speed up a bulk load, a crash or a power loss of s3 can lose commits that s1 was already told about. If gophant runs on the s3 machine it stops with it and has to be started again.

Status of the evidence: the shutdown of s3 while `sync` was stopped and while it ran, the `SIGKILL` of `sync` and s3 together under a write load, the refusal after `applied_lsn` was set back (a rolled-back s3, simulated) and the refusal on an invalidated slot were checked by hand on PostgreSQL 16 and 18 servers in Docker on 2026-11-21, and the rows of s3 equalled s1 after the outages. They are not part of the automated end-to-end suite. Not tested: a silent loss of the network (a broken connection is then noticed only by TCP keepalive, which can take minutes) and changes of the s3 settings other than a restart.

### 7. Many databases at once

A cluster with many databases does not need one configuration and one set of commands per database. With `databases.auto: true` gophant finds the databases of s1 and runs the stages for each of them. [multi_database.md](multi_database.md) describes it in full; this is the short way through.

1. **Configuration.** `source.dsn` and `target.dsn` name the `postgres` database (where gophant connects first); add the `databases` section, as in [configs/gophant.example.yaml](../configs/gophant.example.yaml).
2. **s1.** The `pg_hba.conf` lines of the role say `all` instead of one database name, and there is a line for `replication`. `max_replication_slots` and `max_wal_senders` cover the number of databases and a few more. A publication is needed in every database: gophant creates it when the source role is a superuser, otherwise the DBA runs the script that `precheck` prints.
3. **s3.** The role is a superuser (or has `CREATEDB` and may `SET session_replication_role`): gophant creates the databases that s3 lacks, with the encoding and collation of s1. The roles that own objects are created once for the whole cluster (`pg_dumpall --roles-only`); `precheck` lists the ones that are missing.
4. **Run it.**

| Step | Command | What happens |
|---|---|---|
| 1 | `gophant precheck -c gophant.yaml` | One report: the checks of the cluster once, then every database (ready, blocked or skipped), and the statements that gophant may not run itself. |
| 2 | `gophant run -c gophant.yaml` | Creates what is missing, copies the databases (`databases.parallel` at a time) and streams each one as soon as its copy is done. It keeps running. A database with a failed check is skipped after you agreed (`--yes`). |
| 3 | Stop all writes to s1, then in another terminal `gophant cutover -c gophant.yaml` | All databases are cut over at the same time; `run` ends by itself. |
| 4 | `gophant verify -c gophant.yaml`, switch the applications, `gophant verify --drop-slot -c gophant.yaml` | As for one database, for every database. |

`gophant status` shows one line per database. `-d <name>` limits any command to some databases, for example to cut over the databases of one application first. After an interruption run `gophant run` again: a database that streams already is not copied again. `gophant snapshot` and `gophant sync` work on all databases as well, if you prefer the steps apart; the slot of the first database then waits for all the others, so size `max_slot_wal_keep_size` as described in [multi_database.md](multi_database.md).

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

`max_slot_wal_keep_size` 要按"直到 `sync` 开始消费槽"的整个时长来估算：拷贝时间，**加上** post-data 导入（索引在 s3 上建完之后 `sync` 才能启动），再加上追赶时间。这段时间里 s1 一直在写 WAL，累积量超过上限时槽会失效（`wal_status = lost`），快照必须重做。事先测量 s1 的 WAL 速率：在业务繁忙时段，间隔一小时两次读取 `SELECT wal_bytes FROM pg_stat_wal;`。至少按预计量的 1.5 到 2 倍规划，并让该上限小于 s1 的剩余磁盘，因为上限只是封顶：磁盘比上限小的话，会先写满。`wal_level = logical` 本身产生的 WAL 也比 `replica` 略多。

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

全局参数：`-c/--config`（默认 `gophant.yaml`）、`-d/--database <name>`（仅在设置了 `databases.auto` 时有效，可重复，见第 7 节）、`--log-level debug|info|warn|error`、`--log-format json|text`。报告输出到 stdout，日志输出到 stderr。

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
| `databases.auto` | `false` | 迁移 s1 的所有数据库，而不是一个，见第 7 节。 |
| `databases.include`、`databases.exclude` | 发现的全部、无 | 要迁移的库和永不迁移的库：库名或带 `*`、`?` 的模式。 |
| `databases.parallel` | 1 | 同时做 snapshot（和 verify）的库数。 |
| `databases.create_target`、`databases.create_publication` | `true`、`true` | 创建 s3 上缺少的库，以及 s1 上缺少的 publication（需要 s1 上的超级用户）。 |
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
| s3 被关机、重启或崩溃 | 启动 s3。仍在运行的 `sync` 会自行重试并继续；已停止的用 `gophant sync` 再次启动。不丢也不会重复回放。s3 停机期间 s1 会保留 WAL，能停多久见第 6 节的最后一个问题。 |
| `sync` 提示槽已超过 s3 的位点 | s3 丢失了状态（恢复备份、重置了 `gophant_meta`）。缺失的 WAL 已不存在：执行 `gophant snapshot --restart`。 |
| `sync` 提示槽已失效 | `sync` 停止期间超过了 `max_slot_wal_keep_size`。执行 `gophant snapshot --restart`，并提高限制或让 `sync` 保持运行。 |
| 割接超时 | 信息中给出 lag、打开的写事务数以及 `sync` 是否在运行。阶段保持 `CUTOVER`；修复原因后再次运行 `cutover`。 |
| `verify` 发现差异 | 不要切换应用。报告会指出表和主键区间。 |
| `--restart` | 删除槽、清空已拷贝的表、重置状态、重新拷贝。它会请求确认；`--yes` 跳过询问。 |

### 6. 常见问题

**可以先把 s1 的备份恢复到 s3，再开始复制吗？** 不可以，也没有必要。`snapshot` 的拷贝就是全量恢复：它创建槽、导出快照，并在该快照下并发拷贝所有表，期间 s1 保持在线。逻辑复制只能从它的槽的位点继续，单独恢复出来的备份没有可以接续的位点：槽在备份之后创建，中间的变更会丢；槽在备份之前创建，这些变更会到达两次。设置 `snapshot.skip_ddl = true` 时，表必须存在**并且为空**；表里有数据时 `precheck` 会失败。如果必须使用你自己的恢复流程，就得先创建槽，再用 `pg_dump --snapshot=<导出的快照名>` 导出；gophant 目前没有这种模式。

**gophant 在哪里运行？s3 必须在 Docker 里吗？** 不必。Docker 只是端到端测试的搭建方式。gophant 是不依赖 CGo 的单个二进制，可以在任何既能连到 s1（SQL 和复制连接）和 s3、又装有 PostgreSQL 18 的 `pg_dump` 和 `pg_restore` 的机器上运行（把 `tools.pg_dump_bin` 和 `tools.pg_restore_bin` 设置为它们的路径）。拷贝的行会流经 gophant 进程，所以该机器到两台服务器的链路都要好；s3 所在机器是自然的选择，这样只剩一跳网络（s1 到 gophant）。在 s3 上要在 `pg_hba.conf` 中放行 gophant 所在机器，并使用超级用户（或被允许 `SET session_replication_role`、创建扩展、修改 owner 的角色）。连接串里的 TLS 参数会传给 `pg_dump` 和 `pg_restore`，但没有测试过。

**长时间拷贝对 s1 有什么代价？** s1 保持可写。拷贝期间每个 worker 都持有一个 `REPEATABLE READ` 事务，所以 `VACUUM` 无法清理快照之后产生的死行；更新频繁的表会一直膨胀到拷贝结束。每个 worker 也都在读 s1，s1 繁忙时请调低 `snapshot.workers`。s1 的 DDL 不能变化，槽还会拦住 WAL 的回收（见第 1 节）。

**快照完成之后，s3 可以关机或重启几个小时吗？要全部重新拷贝吗？** 不用。`applied_lsn` 与数据行在 s3 的同一个事务里提交，而 s1 只在 s3 提交之后才推进槽，所以 s3 的关机、重启甚至崩溃都不会丢数据，也不会重复回放：`sync` 从 `applied_lsn` 继续。一直运行的 `sync` 会自行重试（间隔从 1 秒增长到 30 秒，每次尝试都记一条警告），直到 s3 恢复。已经停止的 `sync`，等 s3 能连接之后用 `gophant sync` 再次启动；启动得太早，它会立刻以连接错误退出，不改变任何东西。不要再运行 `gophant snapshot`：已完成的快照会被拒绝，除非加 `--restart`。

代价在 s1 一侧。s3 停机期间没有任何东西消费槽，所以 s1 会保留自 s3 最后一次提交的位点以来写下的全部 WAL，`max_slot_wal_keep_size` 就是 s3 最多能停多久的上限：剩余空间除以 s1 的 WAL 速率（测量方法见第 1 节）就是可停的小时数。下面的查询显示剩余空间（上限为无限或槽已失效时为空）。预计停机较长时，先在 s1 上调大这个上限（reload 即可），但不要超过 s1 的剩余磁盘。

SQL 与英文版相同，见上面的代码块。

超过上限时槽会失效（`wal_status = lost`），`sync` 会拒绝启动并提示执行 `snapshot --restart`，也就是全部重新拷贝。s3 丢失了进度时同样需要重新拷贝：s3 从更早的备份或虚拟机快照恢复、`gophant_meta` 被删除、或数据库被删除后重建。`sync` 能发现 s3 被回退（槽超前于 `applied_lsn`），并拒绝启动，而不是悄悄跳过变更。s3 重建后在 s1 上遗留的旧槽会让 `precheck` 报 FAIL；在 s1 上用 `SELECT pg_drop_replication_slot('gophant_slot')` 删除它（或使用 `source.slot_name` 中的名字）。

s3 停机或还在配置期间，不要改动 s3 上已迁移的表的行或列：`sync` 要求 s1 的每一行在 s3 上恰好出现一次，遇到第一个对不上的就会停止（主键重复、UPDATE 找不到行、s3 缺少某一列）。创建角色、安装扩展、修改服务器参数不会触及回放的行；gophant 在 s3 上使用的角色必须保留登录权限和 `SET session_replication_role` 的权限。s3 上请保持 `fsync = on`（默认值）：回放器让每次提交都是持久的（`synchronous_commit = on`），但 `fsync = off`（有时为加快批量导入而关闭）时，s3 崩溃或断电可能丢失 s1 已经确认收到的提交。如果 gophant 运行在 s3 所在机器上，它会随 s3 一起停止，需要再启动。

验证情况：`sync` 停止时和运行时分别让 s3 关机、在写入负载下把 `sync` 和 s3 一起 `SIGKILL`、把 `applied_lsn` 调回去之后的拒绝启动（模拟 s3 被回退）、槽失效时的拒绝启动，这几项于 2026-11-21 在 Docker 中的 PostgreSQL 16 和 18 服务器上手工检查过，停机之后 s3 的数据行与 s1 一致。它们不属于自动化的端到端测试套件。未测试：网络静默中断（断开的连接只能靠 TCP keepalive 发现，可能需要几分钟），以及重启以外的 s3 参数修改。

### 7. 一次迁移多个数据库

有很多数据库的集群，不需要每个库一份配置和一套命令。设置 `databases.auto: true` 之后，gophant 会发现 s1 上的数据库，并对每个库运行各个阶段。完整说明见 [multi_database.md](multi_database.md)；这里是简短的走法。

1. **配置。** `source.dsn` 和 `target.dsn` 写 `postgres` 库（gophant 最先连接的库）；加上 `databases` 配置段，写法见 [configs/gophant.example.yaml](../configs/gophant.example.yaml)。
2. **s1。** 角色的 `pg_hba.conf` 条目写 `all` 而不是某一个库名，并有一条 `replication` 的条目。`max_replication_slots` 和 `max_wal_senders` 要覆盖库的数量再多几个。每个库都需要一个 publication：源角色是超级用户时由 gophant 创建，否则由 DBA 执行 `precheck` 打印的脚本。
3. **s3。** 角色是超级用户（或有 `CREATEDB` 并且可以 `SET session_replication_role`）：s3 上缺少的库由 gophant 创建，使用 s1 的编码和排序规则。拥有对象的角色对整个集群只需创建一次（`pg_dumpall --roles-only`）；`precheck` 会列出缺少的角色。
4. **执行。**

| 步骤 | 命令 | 发生什么 |
|---|---|---|
| 1 | `gophant precheck -c gophant.yaml` | 一份报告：集群的检查只显示一次，然后是每个库（ready、blocked 或 skipped），以及 gophant 不允许自己执行的语句。 |
| 2 | `gophant run -c gophant.yaml` | 创建缺少的东西，拷贝各个库（同时做 `databases.parallel` 个），每个库一拷完就开始它的流。它持续运行。检查失败的库在你同意之后（`--yes`）被跳过。 |
| 3 | 停止对 s1 的所有写入，然后在另一个终端运行 `gophant cutover -c gophant.yaml` | 所有库同时割接；`run` 会自行结束。 |
| 4 | `gophant verify -c gophant.yaml`，切换应用，`gophant verify --drop-slot -c gophant.yaml` | 与单库相同，对每个库执行。 |

`gophant status` 每个库显示一行。`-d <name>` 把任何命令限定在某几个库上，例如先割接某个应用的库。中断之后再运行一次 `gophant run`：已经在流的库不会被重新拷贝。如果你想把各步分开，`gophant snapshot` 和 `gophant sync` 同样对所有库工作；这时第一个库的槽要等其他所有库，所以请按 [multi_database.md](multi_database.md) 中的说明估算 `max_slot_wal_keep_size`。
