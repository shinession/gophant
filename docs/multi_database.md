# English

## Multi-database mode (`databases.auto`)

One configuration used to describe one pair of databases, so a cluster with many databases needed one configuration and one set of commands per database. With `databases.auto: true` gophant finds the databases of s1 itself and runs the stages for each of them. Code: [internal/multidb](../internal/multidb); the stages themselves are the ones of the single-database mode and are not changed.

### How it works

Logical replication belongs to one database: a replication slot is created in a database (its name is unique in the whole cluster) and a publication lives in one database. The mode therefore does not replace the stages, it repeats them.

1. **Discovery.** `source.dsn` and `target.dsn` name the database that gophant connects to first (use `postgres`). The databases are read from `pg_database` of s1: not a template, connections allowed. `databases.include` and `databases.exclude` (exact names or patterns with `*` and `?`, case sensitive, `exclude` wins) narrow the list, and `--database <name>` (`-d`, repeatable) narrows it for one command. A name that does not exist or is left out by the configuration is an error.
2. **One configuration per database.** For each database gophant derives a copy of the configuration with the database name replaced in `source.dsn`, `source.replication_dsn` and `target.dsn`, and with its own slot. The filter, snapshot, cdc, cutover and verify settings are shared. The publication keeps its name (`source.publication_name`), because a publication belongs to one database anyway.
3. **State stays where it was.** The state of a database is the `gophant_meta` schema of that database on s3. Nothing else keeps state, so running a command again picks up where the last one stopped, database by database.

The slot of a database is `<source.slot_name>_<database>`. A slot name may only hold lower-case letters, digits and underscores and has at most 63 characters, so any other character is replaced by `_`, and a name that had to be changed, or is too long, gets a hash of the original name appended (`Sales-2024` becomes `gophant_slot_sales_2024_<8 hex digits>`, and `Sales` and `sales` stay two slots). `source.slot_name` is therefore limited to 40 characters in this mode.

### Configuration

```yaml
source:
  dsn: "postgres://gophant_repl:${S1_PASSWORD}@s1-host:5432/postgres?sslmode=prefer"
  slot_name: "gophant_slot"
  publication_name: "gophant_pub"
target:
  dsn: "postgres://postgres:${S3_PASSWORD}@s3-host:5432/postgres?sslmode=prefer"

databases:
  auto: true
  exclude: ["postgres"]   # optional
  parallel: 2             # snapshots at the same time
```

| Key | Default | Meaning |
|---|---|---|
| `databases.auto` | `false` | Switches the mode on. Without it nothing changes: the database in `source.dsn` and `target.dsn` is migrated, as before. |
| `databases.include` | all found | Databases to migrate: names or patterns with `*` and `?`. |
| `databases.exclude` | none | Databases that are never migrated. |
| `databases.parallel` | `1` | How many databases are snapshotted (and verified) at the same time. The copy connections on s1 are this number times `snapshot.workers`. |
| `databases.create_target` | `true` | Create a database that s3 does not have yet. |
| `databases.create_publication` | `true` | Create the publication in a database of s1 that does not have it, when the source role is a superuser. |

`source.replication_dsn` is optional here: the replication parameter is added to `source.dsn`. If you set it, its database name is replaced like the others.

### The commands

| Command | In the multi-database mode |
|---|---|
| `precheck` | Checks the cluster once and every database, prints one report (below). Exit code 1 on any FAIL. |
| `snapshot` | One precheck of all databases, then for each database that passed: create what is missing, copy. `databases.parallel` at a time. A database whose snapshot is complete is skipped. `--restart` asks once for all databases. |
| `sync` | The streams of all databases whose snapshot is done, in one process. A database that is not ready is reported and left out; a stream that fails permanently stops alone. |
| `run` | `precheck`, `snapshot` and `sync` in one command, as a pipeline: the stream of a database starts as soon as its own snapshot is done, while the next databases are still being copied. Keeps running until the cutover has ended every stream. Run it again after an interruption: a database that streams already is not copied again. |
| `cutover` | Asks once, then runs the cutover of all selected databases at the same time. Refuses to start while the snapshot of a selected database is not finished (`--database` limits the cutover). A database that is already cut over is skipped. |
| `verify` | Verifies the databases, `databases.parallel` at a time. The full report is printed for a database with differences, and when only one database is selected; the others get one line. `--drop-slot` drops the slot of every verified database. |
| `status` | One line per database: stage, lag, slot, tables copied. With `--database` a single database is shown in full. Needs s1, because the databases are found there. |

A database without a table in scope is skipped by every command: it never gets a migration state, and the commands tell it from a database that has tables and never started. That one stops the `cutover` before it begins, and is reported as failed by `sync` (whose other streams go on) and by `verify`.

Every stage prints a summary of the databases at its end and exits with 1 when one of them failed. The text that a stage prints for one database has the database name in front of every line, and the log lines carry `db=<name>`.

### The precheck report

The checks that have one answer for the cluster (versions, `wal_level`, `max_slot_wal_keep_size`, the replication role, the capacity for slots and walsenders, the tools, the missing roles) are shown once; the others are shown per database, and only when they are not a PASS. A table lists every database with its tables, size, whether s3 has it, and the result:

* **ready**: no FAIL (warnings are listed).
* **BLOCKED**: at least one FAIL of its own. `run` and `snapshot` skip it after you agreed (`--yes` agrees) and start the others; a failure of the cluster stops everything.
* **skipped**: no table in scope. Nothing is copied for it, and it does not count against anything.

The missing roles of all databases are merged into one list. The capacity of s1 is checked once, for the databases that will be migrated: a slot for each one that has none and a walsender for each one that is not being consumed. A database whose copy of s3 does not exist yet is checked as if it were an empty one (`target.database` says what happens to it).

What gophant may not or cannot do for you is printed as a script at the end: the `CREATE PUBLICATION` statements for the databases of s1 (with `\connect`, to run as a superuser; held back, commented out, for a database whose `source.replica_identity` fails, because a publication makes UPDATE and DELETE fail on such a table), and the `CREATE DATABASE` statements for s3 when `databases.create_target` is `false`.

### What gophant creates

* **The database on s3**, when it does not exist and `databases.create_target` allows it: `CREATE DATABASE ... TEMPLATE template0` with the encoding, `LC_COLLATE`, `LC_CTYPE` and the locale provider (C library or ICU) of s1, and the owner of s1 when that role exists on s3 (otherwise the connecting role owns it, and the precheck warns). The statement is built by the server with `format()`, so no name is pasted into SQL. The role needs `CREATEDB` (a superuser has it). gophant never drops a database.
* **The publication in a database of s1**, when it does not exist, `databases.create_publication` allows it and the source role is a superuser: `CREATE PUBLICATION ... FOR ALL TABLES WITH (publish_via_partition_root = true)`. It happens only after the precheck of that database passed, including `source.replica_identity`. A publication that exists but is wrong is a FAIL and is never changed.

Not migrated, as in the single-database mode: roles (the precheck lists the ones that are missing on s3; create them with `pg_dumpall --roles-only`), tablespaces, large objects, and what belongs to the database itself and not to its schema: `ALTER DATABASE ... SET` settings, the privileges on the database (`GRANT CONNECT`), its comment. A database without any table is skipped, so its other objects (functions, views) are not migrated either.

### The WAL that s1 keeps

A slot holds back WAL on s1 from the moment it is created until it is consumed. With `gophant snapshot` followed by `gophant sync`, the slot of the first database waits until the last database is copied, so `max_slot_wal_keep_size` has to cover all copies together. With `gophant run` a slot waits only for its own database (copy, post-data import, catch-up), so it has to cover the longest single database. When a snapshot fails, its slot stays on s1 until `gophant snapshot --restart --database <name>` drops it; the log says so, and the limit bounds the damage.

### Resources and prerequisites

* `pg_hba.conf` on s1: the role needs the lines for every database, not for one (`all` instead of the database name), and for `replication`. On s3 the connecting role needs `CREATEDB` (or be a superuser) to create databases.
* Every database that streams holds one replication connection on s1 (`max_wal_senders`) and one slot (`max_replication_slots`), and a few ordinary connections on both servers (each pool allows up to four). Size `max_connections` for the number of databases. A snapshot in progress holds the walsender of its slot as well.
* The memory limit `cdc.max_tx_memory_mb` is per stream.

### Limits

* The filter settings are the same for all databases. There are no overrides per database.
* `status` needs s1.
* Not tested: a database whose locale provider is ICU (the code builds `LOCALE_PROVIDER icu ICU_LOCALE`), an encoding other than UTF8, and more than seven databases.

### Lessons learned

* Count the capacity of s1 for the databases that will be migrated, not for those that were found: every database without a table (`postgres` is usually one) would otherwise need a slot and a walsender, and a cluster with many small or empty databases would fail the precheck for no reason.
* A slot name is unique in the cluster and case folded, a database name is neither: "Sales" and "sales" are two databases. The hash in the slot name is what keeps them apart.
* The precheck of a database that s3 does not have yet cannot look inside it. It runs against another database of s3 for what needs a server, and assumes an empty database for the rest (`precheck.Options.TargetMissing`).
* A snapshot that runs with `SkipPrecheck` must be started only after the precheck of the same run: the multi-database mode checks all databases once and must not print a second report for each of them.

---

# 中文

## 多库模式（`databases.auto`）

以前一份配置只描述一对数据库，所以一个有很多数据库的集群，每个库都要一份配置和一套命令。设置 `databases.auto: true` 之后，gophant 自己发现 s1 上的数据库，并对每个库运行各个阶段。代码在 [internal/multidb](../internal/multidb)；各个阶段本身就是单库模式的阶段，没有改动。

### 工作方式

逻辑复制属于某一个数据库：复制槽是在某个数据库里创建的（槽名在整个集群中唯一），publication 也只在一个数据库中。所以这个模式没有替换各个阶段，而是把它们重复运行。

1. **发现。** `source.dsn` 和 `target.dsn` 里的库名只是 gophant 最先连接的那个库（请用 `postgres`）。数据库列表来自 s1 的 `pg_database`：不是模板库、允许连接。`databases.include` 和 `databases.exclude`（精确库名或带 `*`、`?` 的模式，区分大小写，`exclude` 优先）缩小范围，`--database <name>`（`-d`，可重复）为单条命令缩小范围。不存在或被配置排除的库名会报错。
2. **每个库一份配置。** 对每个库，gophant 派生一份配置的副本：`source.dsn`、`source.replication_dsn` 和 `target.dsn` 中的库名被替换，并使用该库自己的槽。filter、snapshot、cdc、cutover 和 verify 的设置是共用的。publication 沿用 `source.publication_name` 的名字，因为 publication 本来就属于某一个库。
3. **状态仍在原来的地方。** 一个库的状态就是它在 s3 上的 `gophant_meta` schema。没有别的地方保存状态，所以再次运行命令会逐库接着上一次停下的地方继续。

一个库的槽是 `<source.slot_name>_<库名>`。槽名只能包含小写字母、数字和下划线，最长 63 个字符，所以其他字符会被替换成 `_`；被改动过或太长的名字，会在末尾追加原库名的哈希（`Sales-2024` 变成 `gophant_slot_sales_2024_<8 位十六进制>`，`Sales` 和 `sales` 仍是两个槽）。因此该模式下 `source.slot_name` 最长 40 个字符。

### 配置

配置示例与英文版相同，见上面的代码块。

| 配置项 | 默认值 | 含义 |
|---|---|---|
| `databases.auto` | `false` | 打开该模式。不设置则什么都不变：迁移 `source.dsn` 和 `target.dsn` 中的那个库，和以前一样。 |
| `databases.include` | 发现的全部 | 要迁移的库：库名或带 `*`、`?` 的模式。 |
| `databases.exclude` | 无 | 永不迁移的库。 |
| `databases.parallel` | `1` | 同时做 snapshot（和 verify）的库数。s1 上的拷贝连接数是它乘以 `snapshot.workers`。 |
| `databases.create_target` | `true` | s3 上还没有的库由 gophant 创建。 |
| `databases.create_publication` | `true` | s1 的某个库没有 publication 时，若源角色是超级用户，则由 gophant 创建。 |

这里 `source.replication_dsn` 可以省略：会在 `source.dsn` 上加复制参数。如果设置了它，其中的库名也会像其他连接串一样被替换。

### 各条命令

| 命令 | 在多库模式下 |
|---|---|
| `precheck` | 检查一次集群和每个库，输出一份报告（见下）。有 FAIL 时退出码为 1。 |
| `snapshot` | 先对所有库做一次预检，然后对通过的库：创建缺少的东西，拷贝。同时做 `databases.parallel` 个库。snapshot 已完成的库会被跳过。`--restart` 对所有库只询问一次。 |
| `sync` | 在一个进程里运行所有 snapshot 已完成的库的流。没准备好的库会被报告并排除；永久失败的流只停止它自己。 |
| `run` | 把 `precheck`、`snapshot` 和 `sync` 合成一条命令，按流水线运行：一个库自己的 snapshot 一完成，它的流就启动，同时后面的库还在拷贝。一直运行到割接结束了所有的流。中断之后再运行一次：已经在流的库不会被重新拷贝。 |
| `cutover` | 只询问一次，然后同时对所有选中的库割接。选中的库中有 snapshot 未完成的，就拒绝开始（用 `--database` 限定范围）。已经割接完成的库会被跳过。 |
| `verify` | 同时校验 `databases.parallel` 个库。有差异的库，以及只选中一个库时，会打印完整报告；其余的库各占一行。`--drop-slot` 会删除每个校验通过的库的槽。 |
| `status` | 每个库一行：阶段、lag、槽、已拷贝的表。用 `--database` 时完整显示那一个库。需要能连上 s1，因为库是在 s1 上发现的。 |

范围内没有表的库会被每条命令跳过：它不会有迁移状态，命令会把它与"有表但从未启动"的库区分开。后者会让 `cutover` 在开始之前就停下，并被 `sync`（其他的流继续运行）和 `verify` 报告为失败。

每个阶段结束时都会打印各库的汇总，只要有一个库失败，就以 1 退出。阶段为某个库打印的文字，每一行前面都有库名，日志行带有 `db=<name>`。

### 预检报告

对整个集群只有一个答案的检查（版本、`wal_level`、`max_slot_wal_keep_size`、复制角色、槽和 walsender 的容量、工具、缺失的角色）只显示一次；其余的按库显示，并且只显示不是 PASS 的。一张表列出每个库的表数、大小、s3 上是否已有、以及结果：

* **ready**：没有 FAIL（警告会列出）。
* **BLOCKED**：自己有至少一个 FAIL。`run` 和 `snapshot` 在你同意之后（`--yes` 表示同意）跳过它，启动其他的库；集群本身的失败则会停止一切。
* **skipped**：范围内没有表。不为它拷贝任何东西，也不影响别的库。

所有库缺失的角色会合并成一份清单。s1 的容量只检查一次，针对将要迁移的库：每个还没有槽的库需要一个槽，每个槽没有被消费的库需要一个 walsender。s3 上还不存在的库，按一个空库来检查（`target.database` 说明会对它做什么）。

gophant 不能或不允许替你做的事，会在末尾以脚本形式打印：s1 各库的 `CREATE PUBLICATION` 语句（带 `\connect`，以超级用户运行；对 `source.replica_identity` 失败的库会被注释掉并暂缓，因为 publication 一旦存在，这样的表上的 UPDATE 和 DELETE 就会失败），以及在 `databases.create_target` 为 `false` 时 s3 的 `CREATE DATABASE` 语句。

### gophant 会创建什么

* **s3 上的库**：库不存在且 `databases.create_target` 允许时，执行 `CREATE DATABASE ... TEMPLATE template0`，使用 s1 的编码、`LC_COLLATE`、`LC_CTYPE` 和 locale provider（C 库或 ICU），并在 s3 上存在该角色时使用 s1 的 owner（否则由连接角色拥有，预检会警告）。语句由服务器用 `format()` 生成，所以没有任何名字被拼进 SQL。角色需要 `CREATEDB`（超级用户具备）。gophant 从不删除数据库。
* **s1 某个库中的 publication**：它不存在、`databases.create_publication` 允许且源角色是超级用户时，执行 `CREATE PUBLICATION ... FOR ALL TABLES WITH (publish_via_partition_root = true)`。只会在该库的预检通过之后进行，包括 `source.replica_identity`。已经存在但不正确的 publication 是 FAIL，不会被修改。

与单库模式一样不会迁移：角色（预检会列出 s3 上缺的角色，请用 `pg_dumpall --roles-only` 创建）、表空间、大对象，以及属于数据库本身而不属于其 schema 的东西：`ALTER DATABASE ... SET` 设置、库上的权限（`GRANT CONNECT`）、库的注释。没有任何表的库会被跳过，所以它的其他对象（函数、视图）也不会被迁移。

### s1 保留的 WAL

槽从创建那一刻起到被消费之前，会让 s1 一直保留 WAL。先用 `gophant snapshot` 再用 `gophant sync` 时，第一个库的槽要等到最后一个库拷完，所以 `max_slot_wal_keep_size` 必须覆盖所有拷贝的总时长。用 `gophant run` 时，一个槽只等它自己的库（拷贝、post-data 导入、追赶），所以只需覆盖最长的那个库。某个 snapshot 失败时，它的槽会留在 s1 上，直到 `gophant snapshot --restart --database <name>` 把它删除；日志会这样提示，而上限限定了损失的范围。

### 资源与前置条件

* s1 的 `pg_hba.conf`：角色需要的是所有库的配置行，而不是某一个库（用 `all` 代替库名），以及 `replication` 的配置行。在 s3 上，连接角色需要 `CREATEDB`（或是超级用户）才能创建数据库。
* 每个在流的库占用 s1 上一个复制连接（`max_wal_senders`）和一个槽（`max_replication_slots`），以及两台服务器上的几个普通连接（每个连接池最多四个）。请按库数估算 `max_connections`。正在做 snapshot 的库同样占用其槽的 walsender。
* 内存限制 `cdc.max_tx_memory_mb` 是每个流各自的。

### 限制

* 所有库使用相同的 filter 设置，不能按库覆盖。
* `status` 需要连得上 s1。
* 未测试：locale provider 是 ICU 的库（代码会生成 `LOCALE_PROVIDER icu ICU_LOCALE`）、UTF8 以外的编码，以及超过七个库的情况。

### 经验教训

* s1 的容量要按将要迁移的库来算，而不是按发现的库来算：否则每个没有表的库（`postgres` 通常就是）都要占一个槽和一个 walsender，有很多小库或空库的集群会无缘无故地预检失败。
* 槽名在集群内唯一且会被转成小写，而库名不是：`Sales` 和 `sales` 是两个库。槽名里的哈希就是用来区分它们的。
* 对 s3 上还不存在的库做预检时，无法看它的内部。需要服务器的检查在 s3 的另一个库上运行，其余的按空库假设（`precheck.Options.TargetMissing`）。
* 带 `SkipPrecheck` 运行的 snapshot 必须在同一次运行的预检之后才启动：多库模式只对所有库检查一次，不能为每个库再打印一份报告。
