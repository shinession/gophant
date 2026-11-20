# English

## Stream replay: `sync`, the reader, the buffer and the applier

`gophant sync` is stage 3. It consumes the `pgoutput` stream of the replication slot and replays every committed transaction on s3 exactly once. Code: [internal/cdc](../internal/cdc) (decoder, transaction buffer, reader, heartbeat), [internal/applier](../internal/applier) (SQL builder, replay) and [internal/syncer](../internal/syncer) (wiring, reconnects, shutdown).

### Data flow

```text
s1 walsender --pgoutput v1--> Reader --Event--> Applier --one s3 transaction--> s3
                                |  ^                              |
                                |  +------ Progress.Flushed ------+
                                +--> Heartbeat --StandbyStatusUpdate--> s1
```

* **Reader** decodes the messages, keeps the relation map and assembles a transaction between `Begin` and `Commit`. Tables outside the filter are dropped while decoding. A transaction is delivered if its `CommitLSN >= position`; older ones were applied already.
* **Transaction buffer** keeps the changes of a transaction in memory up to `cdc.max_tx_memory_mb`, then moves all of it to a temporary file in `cdc.temp_dir` and appends to the file. Transactions that wait for the applier are limited to about twice that size: a slow s3 slows the reader down.
* **Applier** merges transactions into micro-batches (`cdc.batch_max_txns`, `cdc.batch_max_delay`, at most 20000 changes), builds the statements, sends them with `pgx.Batch` inside one s3 transaction and commits. A transaction that was spilled is applied on its own.
* **Heartbeat** reports `Progress.Flushed` to s1 every `cdc.standby_status_interval` and whenever s1 asks for a reply. It is its own goroutine, so a busy applier never causes a walsender timeout.

### Positions

| Name | Meaning |
|---|---|
| `consistent_lsn` | Returned by the slot creation. The snapshot equals the state of s1 at this point. |
| `applied_lsn` | Stored on s3. **Exclusive end position**: every transaction whose commit record starts before it is applied. It is the `TransactionEndLSN` of the last applied transaction, or the idle position (below). |
| start position | `applied_lsn`, or `consistent_lsn` when it is empty. `START_REPLICATION` begins there. |
| `Flushed` | The position that may be reported to s1. It moves only after the s3 transaction that stored `applied_lsn` has committed. s1 may delete WAL before it. |

**Exactly-once.** The business statements and `UPDATE sync_state SET applied_lsn = ...` are in one s3 transaction. After a crash either both exist or neither; the restart reads `applied_lsn` from s3 and the reader skips what was applied. The update only matches when the stored position is behind the batch; if it matches no row the transaction is rolled back, because the batch would be applied twice. Only after the commit is `Flushed` advanced and reported.

**Idle position.** When nothing is open in the reader, nothing is queued and nothing is in the applier, a keepalive carrying `ServerWALEnd` means that s1 decoded up to there and sent every transaction before it. The reader then hands that position to the applier, which stores it as `applied_lsn`. Without it an idle migration would never reach lag zero, because s1 keeps writing records of its own. While the cutover waits, the heartbeat runs every second.

### How a change is replayed

| Case | Behaviour |
|---|---|
| Session | `session_replication_role = replica` (no triggers, no foreign key checks) and `synchronous_commit = on` (the commit must be durable before the position is reported). |
| `UnchangedTOAST` | A column that arrives as unchanged TOAST (`'u'`) is **never** in the `SET` list, so the value on s3 stays. An update left with nothing to assign is skipped. |
| Generated columns | Never written. s1 does not publish them; s3 computes them. |
| Identity columns | `INSERT` uses `OVERRIDING SYSTEM VALUE` for `GENERATED ALWAYS AS IDENTITY` columns. `UPDATE` never assigns them; a change of the value is detected when the old value is known and fails. |
| Locating a row | By the replica identity columns of the relation message (primary key, identity index, or all columns for `FULL`), from the old tuple if the message has one, otherwise from the new tuple. `NULL` is matched with `IS NULL`. |
| No primary key (`FULL`) | The full old row locates the row, and `WHERE ctid = (SELECT ctid FROM ONLY t WHERE ... LIMIT 1)` makes sure exactly one of several identical rows is changed. For a partitioned root it is `(tableoid, ctid)`. |
| Types without `=` | json, xml and geometric types in the identity are compared as text. |
| Partitions | The publication uses `publish_via_partition_root`, so the root table name arrives and s3 routes the row. No `ONLY` on a partitioned root. |
| `TRUNCATE` | One statement for all tables of the message, with `RESTART IDENTITY` and `CASCADE` as on s1. |
| Results | An INSERT must store 1 row; an UPDATE must find exactly 1 row (otherwise the databases diverged: error); a DELETE that finds no row is a warning, more than 1 is an error. |
| Schema drift | A column that s1 publishes and s3 lacks stops the replay with an error that names it. |

### Reconnects and shutdown

* `sync` reads its state, checks the slot on s1 (exists, plugin, not invalidated, not ahead of `applied_lsn`) and starts the reader and the applier.
* A transient error (network, server restart, deadlock, a slot still held by a dead connection) restarts the attempt after a backoff of 1 s growing to 30 s; the backoff starts again after 5 minutes of stable running. Every attempt reads `applied_lsn` from s3, so no state is carried over. A permanent error (a constraint violation, schema drift, a protocol error) ends `sync` with the reason.
* SIGINT/SIGTERM stops the reader. The applier still commits what was received, the final position is reported to s1, and the process exits with code 0.
* A watcher polls the stage every second: it switches the heartbeat to every second in `CUTOVER`, and stops the stream when the stage is `VERIFY` or `DONE`.

### Lessons learned

* `pglogrepl.SendStandbyStatusUpdate` replaces a flush position of 0 by the write position. Reporting 0 would tell s1 that everything received is durable on s3. The heartbeat never sends before the first real position is known.
* The next commit record can start exactly at the end of the previous one, and the first commit after `consistent_lsn` can start exactly there. Storing end positions and comparing with `>=` is the only form that neither skips nor repeats.
* A keepalive can arrive in the middle of a transaction. The idle position is only used when no transaction is open.
* The stage watcher waited for the stream context, which nobody cancelled when the stream ended with an error: `sync` neither reconnected nor exited. Found by the end-to-end test that terminates the walsender. The reader now cancels the context when it returns.
* Replication connections must set `DateStyle`, `IntervalStyle` and `extra_float_digits` explicitly; `gophant` does, as the built-in subscriber does.

---

# 中文

## 流回放：`sync`、reader、缓冲和回放器

`gophant sync` 是阶段 3。它消费复制槽的 `pgoutput` 流，把每个已提交事务 exactly-once 地回放到 s3。代码：[internal/cdc](../internal/cdc)（解码器、事务缓冲、reader、心跳）、[internal/applier](../internal/applier)（SQL 构建、回放）、[internal/syncer](../internal/syncer)（组装、重连、关闭）。

### 数据流

数据流图与英文版相同，见上面的代码块。

* **Reader** 解码消息，维护 relation 映射，并在 `Begin` 与 `Commit` 之间组装事务。过滤范围之外的表在解码时就丢弃。事务在 `CommitLSN >= position` 时才投递，更早的已经回放过。
* **事务缓冲** 把一个事务的变更保存在内存中，最多 `cdc.max_tx_memory_mb`，超出后把全部内容移到 `cdc.temp_dir` 下的临时文件并继续追加。等待回放器的事务总量被限制在该大小的约两倍：s3 变慢时，reader 随之变慢。
* **回放器** 把事务合并成微批（`cdc.batch_max_txns`、`cdc.batch_max_delay`，最多 20000 条变更），构建语句，用 `pgx.Batch` 在一个 s3 事务内发送并提交。溢写到磁盘的事务单独回放。
* **心跳** 每隔 `cdc.standby_status_interval`，以及 s1 要求回复时，把 `Progress.Flushed` 汇报给 s1。它是独立的 goroutine，回放器再忙也不会造成 walsender 超时。

### 位点

| 名称 | 含义 |
|---|---|
| `consistent_lsn` | 建槽时返回。快照等于 s1 在这一点的状态。 |
| `applied_lsn` | 保存在 s3。**排他的结束位置**：提交记录起点在它之前的事务都已回放。它是最后一个已回放事务的 `TransactionEndLSN`，或者是空闲位点（见下）。 |
| 起始位点 | `applied_lsn`，为空时用 `consistent_lsn`。`START_REPLICATION` 从这里开始。 |
| `Flushed` | 可以汇报给 s1 的位点。它只在保存了 `applied_lsn` 的 s3 事务提交之后才前进。s1 可以删除它之前的 WAL。 |

**Exactly-once。** 业务语句和 `UPDATE sync_state SET applied_lsn = ...` 在同一个 s3 事务里。崩溃后两者要么都在，要么都不在；重启时从 s3 读取 `applied_lsn`，reader 跳过已回放的部分。该更新只在已存储位点落后于该批次时才会命中；如果没有命中任何行，事务会回滚，因为这一批会被回放两次。只有提交之后，`Flushed` 才会前进并被汇报。

**空闲位点。** 当 reader 中没有打开的事务、队列里没有排队的、回放器里也没有在途的，携带 `ServerWALEnd` 的 keepalive 就表示 s1 已解码到该处，并已发送它之前的所有事务。此时 reader 把该位点交给回放器，回放器将其保存为 `applied_lsn`。没有它，空闲的迁移永远无法让 lag 归零，因为 s1 会不断写自己的记录。割接等待期间，心跳每秒运行一次。

### 变更如何回放

| 情形 | 行为 |
|---|---|
| 会话 | `session_replication_role = replica`（不触发触发器、不做外键检查）和 `synchronous_commit = on`（汇报位点之前，提交必须已经持久化）。 |
| `UnchangedTOAST` | 以未变更 TOAST（`'u'`）到达的列**绝不**出现在 `SET` 列表里，s3 上的值因此保留。没有任何可赋值内容的 update 会被跳过。 |
| 生成列 | 从不写入。s1 不发布它们；s3 自己计算。 |
| identity 列 | 对 `GENERATED ALWAYS AS IDENTITY` 列，`INSERT` 使用 `OVERRIDING SYSTEM VALUE`。`UPDATE` 从不为它赋值；已知旧值时，如果值发生变化会被检测到并报错。 |
| 定位行 | 使用 relation 消息中的 replica identity 列（主键、identity 索引，`FULL` 时为所有列）；消息有旧元组时取自旧元组，否则取自新元组。`NULL` 用 `IS NULL` 匹配。 |
| 无主键（`FULL`） | 用完整的旧行定位，并用 `WHERE ctid = (SELECT ctid FROM ONLY t WHERE ... LIMIT 1)` 保证多条完全相同的行中只改一条。分区根表使用 `(tableoid, ctid)`。 |
| 没有 `=` 的类型 | identity 中的 json、xml 和几何类型按文本比较。 |
| 分区表 | publication 使用 `publish_via_partition_root`，到达的是根表名，由 s3 路由到分区。分区根表不使用 `ONLY`。 |
| `TRUNCATE` | 消息中所有表合并成一条语句，`RESTART IDENTITY` 和 `CASCADE` 与 s1 一致。 |
| 结果检查 | INSERT 必须存入 1 行；UPDATE 必须恰好找到 1 行（否则两个库已分叉：报错）；DELETE 找不到行只是警告，超过 1 行是错误。 |
| Schema drift | s1 发布了而 s3 没有的列，会让回放带着列名报错并停止。 |

### 重连与关闭

* `sync` 读取状态，检查 s1 上的槽（存在、插件、未失效、不超前于 `applied_lsn`），然后启动 reader 和回放器。
* 暂时性错误（网络、服务器重启、死锁、槽仍被已死的连接占用）会在退避后重新开始一次尝试，退避从 1 秒增长到 30 秒，稳定运行 5 分钟后重新从头计算。每次尝试都从 s3 读取 `applied_lsn`，不携带任何状态。永久性错误（约束冲突、schema drift、协议错误）会让 `sync` 带着原因结束。
* SIGINT/SIGTERM 停止 reader。回放器仍会提交已经收到的内容，最终位点汇报给 s1，进程以退出码 0 结束。
* 一个观察协程每秒轮询阶段：在 `CUTOVER` 时把心跳切换为每秒一次，在阶段为 `VERIFY` 或 `DONE` 时停止流。

### 经验教训

* `pglogrepl.SendStandbyStatusUpdate` 会把为 0 的 flush 位点替换成 write 位点。汇报 0 等于告诉 s1 已收到的一切都已在 s3 上持久化。心跳在知道第一个真实位点之前从不发送。
* 下一个提交记录可能恰好从上一个的结束位置开始，`consistent_lsn` 之后的第一个提交也可能恰好从那里开始。保存结束位置并用 `>=` 比较，是既不漏也不重的唯一形式。
* keepalive 可能在事务中途到达。只有在没有打开的事务时才使用空闲位点。
* 阶段观察协程等待的是流的 context，而流因错误结束时没有人取消它：`sync` 既不重连也不退出。这是终止 walsender 的端到端测试发现的。现在 reader 返回时会取消该 context。
* 复制连接必须显式设置 `DateStyle`、`IntervalStyle` 和 `extra_float_digits`；`gophant` 与内置订阅端一样这样做。
