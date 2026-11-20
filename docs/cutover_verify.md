# English

## Cutover, verification and status

Code: [internal/cutover](../internal/cutover), [internal/sequence](../internal/sequence), [internal/verify](../internal/verify), [internal/status](../internal/status).

### Cutover (stage 4)

Run it after the writes to s1 have been stopped. gophant does not stop them.

1. The migration must be in stage `CDC_STREAM` (or `CUTOVER`, to run it again). A `gophant sync` must be consuming the slot, otherwise the lag cannot reach zero and the command says so.
2. The operator confirms that all writes are stopped (`--yes` skips the question). The stage becomes `CUTOVER`; `sync` reports its position every second from now on.
3. **Waiting for lag zero.** Once a second the command reads the open write transactions and prepared transactions of s1, `pg_current_wal_lsn()` of s1 and `applied_lsn` of s3. A poll counts as caught up when `applied_lsn` reached the WAL position of the previous poll and there is no open write transaction and no prepared transaction. Three polls in a row finish the wait. After `cutover.lag_zero_timeout` the command fails with the lag, the open transactions and whether `sync` runs; the stage stays `CUTOVER` and the command can be run again.
4. **Sequences.** Every used sequence (`last_value IS NOT NULL`) of the selected schemas is set on s3 with `setval(seq, value, true)`. `value` is `last_value + cutover.sequence_offset` (minus it for a descending sequence), clamped to the bounds of the sequence. A sequence that s3 lacks is reported and skipped.
5. The stage becomes `VERIFY`; `sync` sees it and stops.

Open write transactions can only be detected when the role of the source connection is a superuser or a member of `pg_read_all_stats`; `precheck` warns when it is not, and the cutover then relies on the confirmation of the operator.

### Verification (stage 5)

`gophant verify` compares s1 and s3 table by table (every table of `table_progress`, with `ONLY`, so a partition is compared by its own rows). Run it while s1 is still write-stopped.

* **Counts.** The exact `count(*)` of both sides.
* **Fingerprints** (unless `verify.mode` is `count_only`). A table with a primary key is cut into ranges of `verify.chunk_size` rows by key (`OFFSET` over the key, composite keys compared as `ROW(...)`). Each range is fingerprinted on both sides by the same query: the row count and the sum of 64-bit hashes (the first 16 hex digits of the MD5) of the whole-row text. A sum does not depend on the order and, unlike an XOR, does not cancel two identical rows. `full_hash` takes all ranges, `count_and_sample` a random `sample_ratio` of them (at least one per table). A table without a primary key is hashed as one range in `full_hash` mode and only counted otherwise.
* Both sides run with `TimeZone = 'UTC'`, `DateStyle = 'ISO, MDY'`, `IntervalStyle = 'postgres'`, `extra_float_digits = 1`, `bytea_output = 'hex'`, so the text form of a value does not depend on the settings of a server.
* The result of each table is stored in `table_progress.verified_ok`. The report names the table and the key range of every difference; the command exits with code 1.
* Without `--force` verification needs the stage `VERIFY` or `DONE`. `--drop-slot` needs the same stage and a successful verification: it drops the slot on s1, which releases the WAL it holds, and sets the stage to `DONE`.

### Status

`gophant status` prints the stage, `consistent_lsn`, `applied_lsn`, the current WAL position of s1, the lag in bytes, the slot (`active`, `wal_status`, retained WAL) and per table: primary key, replica identity, copy status, rows copied, verified. `--json` gives the same as JSON. When s1 cannot be reached it still shows what s3 knows. After the cutover the lag is only informational: `sync` has stopped while s1 keeps writing records of its own.

### Several databases

With `databases.auto` ([multi_database.md](multi_database.md)) the three commands work on every selected database. `cutover` asks once and runs the cutovers of all databases at the same time, and refuses to start while the snapshot of one of them is not finished; a database that is already cut over is skipped. `verify` is `verify.RunStage`, the same function the single-database command calls, once per database. `status` prints one line per database (`--database` shows one in full, `--json` gives a list) and needs s1, because the databases are found there.

### Lessons learned

* The WAL position of s1 moves by itself after the last transaction (checkpoints, autovacuum; hundreds of kilobytes after a write burst). Waiting for an exact equality at one instant is fragile; the position of the previous poll is a target that does not move.
* `lag_bytes` after the cutover is not 0, and the end-to-end test must not assert it. The cutover output (`Lag is zero after ...`) is the proof.
* A role with `pg_read_all_data` does not see the transaction ids of other sessions; only `pg_read_all_stats` does.

---

# 中文

## 割接、校验与状态

代码：[internal/cutover](../internal/cutover)、[internal/sequence](../internal/sequence)、[internal/verify](../internal/verify)、[internal/status](../internal/status)。

### 割接（阶段 4）

在停止对 s1 的写入之后运行。gophant 不会替你停止写入。

1. 迁移必须处于 `CDC_STREAM` 阶段（或 `CUTOVER`，用于重新运行）。必须有 `gophant sync` 在消费槽，否则 lag 无法归零，命令会明确告知。
2. 操作员确认所有写入已停止（`--yes` 跳过询问）。阶段变为 `CUTOVER`；从此刻起 `sync` 每秒汇报一次位点。
3. **等待 lag 归零。** 命令每秒读取 s1 上打开的写事务和 prepared transaction、s1 的 `pg_current_wal_lsn()` 以及 s3 的 `applied_lsn`。当 `applied_lsn` 达到上一次轮询时的 WAL 位置、且没有打开的写事务和 prepared transaction 时，这次轮询算作已追平。连续三次轮询满足即结束等待。超过 `cutover.lag_zero_timeout` 后，命令会带着 lag、打开的事务数和 `sync` 是否在运行而失败；阶段保持 `CUTOVER`，可以再次运行命令。
4. **序列。** 所选 schema 中每个已使用的序列（`last_value IS NOT NULL`）在 s3 上用 `setval(seq, value, true)` 设置。`value` 为 `last_value + cutover.sequence_offset`（递减序列则为减去），并限制在序列的边界内。s3 上没有的序列会被报告并跳过。
5. 阶段变为 `VERIFY`；`sync` 看到后停止。

只有当源连接的角色是超级用户或 `pg_read_all_stats` 成员时才能检测打开的写事务；否则 `precheck` 会给出警告，割接此时依赖操作员的确认。

### 校验（阶段 5）

`gophant verify` 逐表比较 s1 和 s3（`table_progress` 中的每张表，带 `ONLY`，所以分区按它自己的行比较）。请在 s1 仍处于停写状态时运行。

* **行数。** 两侧精确的 `count(*)`。
* **指纹**（`verify.mode` 不是 `count_only` 时）。有主键的表按主键切成每片 `verify.chunk_size` 行的区间（对主键做 `OFFSET`，复合主键用 `ROW(...)` 比较）。每个区间在两侧用同一条查询计算指纹：行数，以及整行文本的 64 位哈希（MD5 的前 16 个十六进制数字）之和。求和与顺序无关，而且与 XOR 不同，不会让两条相同的行互相抵消。`full_hash` 取所有区间，`count_and_sample` 随机取 `sample_ratio` 比例的区间（每张表至少一个）。无主键的表在 `full_hash` 模式下作为一个区间计算哈希，否则只计数。
* 两侧都使用 `TimeZone = 'UTC'`、`DateStyle = 'ISO, MDY'`、`IntervalStyle = 'postgres'`、`extra_float_digits = 1`、`bytea_output = 'hex'`，所以值的文本形式不依赖服务器的设置。
* 每张表的结果保存在 `table_progress.verified_ok`。报告会指出每处差异的表和主键区间；命令以退出码 1 结束。
* 没有 `--force` 时，校验要求阶段为 `VERIFY` 或 `DONE`。`--drop-slot` 要求同样的阶段并且校验成功：它删除 s1 上的槽，释放它保留的 WAL，并把阶段设为 `DONE`。

### 状态

`gophant status` 输出阶段、`consistent_lsn`、`applied_lsn`、s1 当前的 WAL 位置、以字节计的 lag、槽（`active`、`wal_status`、保留的 WAL），以及每张表的主键、replica identity、拷贝状态、已拷贝行数和校验结果。`--json` 以 JSON 输出相同内容。无法连接 s1 时，仍会显示 s3 所知道的信息。割接之后 lag 仅供参考：`sync` 已停止，而 s1 仍在写自己的记录。

### 多个数据库

设置 `databases.auto`（[multi_database.md](multi_database.md)）后，这三条命令对每个选中的库工作。`cutover` 只询问一次，同时运行所有库的割接，并且在其中一个库的 snapshot 未完成时拒绝开始；已经割接完成的库会被跳过。`verify` 就是 `verify.RunStage`，即单库命令调用的同一个函数，每个库调用一次。`status` 每个库输出一行（`--database` 完整显示一个库，`--json` 给出列表），并且需要连得上 s1，因为库是在 s1 上发现的。

### 经验教训

* 最后一个事务之后，s1 的 WAL 位置会自己移动（checkpoint、autovacuum；一次写入高峰之后可达数百 KB）。在某一时刻等待严格相等很脆弱；上一次轮询的位置是一个不会移动的目标。
* 割接之后 `lag_bytes` 不是 0，端到端测试不能断言它。割接输出里的 `Lag is zero after ...` 才是证明。
* 拥有 `pg_read_all_data` 的角色看不到其他会话的事务号；只有 `pg_read_all_stats` 可以。
