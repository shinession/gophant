# English

## Testing

### Unit tests

`go test ./...` needs no database and no Docker. What they cover:

| Package | Covered |
|---|---|
| `internal/config` | defaults, `${NAME}` expansion, unknown keys, validation messages, the table filter |
| `internal/pg` | splitting the password out of a connection string (URL and keyword form), no leak in errors |
| `internal/meta` | the DDL, stage values, start position, the guarded `applied_lsn` statement |
| `internal/catalog` | which column types are safe for binary COPY (arrays, composites, domains, ranges, recursion) |
| `internal/precheck` | the judgement of every rule, the report and its output |
| `internal/schema` | `pg_dump`/`pg_restore` arguments (the password is never an argument), the restore list filters for pre-data and post-data |
| `internal/snapshot` | the COPY statements, quoting of hostile identifiers, order of the tables, the snapshot name check |
| `internal/cdc` | `pgoutput` messages built byte by byte and decoded, the scope filter, the spill file (order, relation versions, truncates, cleanup), the progress counters |
| `internal/applier` | the SQL builder: ordinary table, unchanged TOAST, composite key, key change, no primary key with `NULL` and duplicates, generated and identity columns, partitioned root, text comparison, truncate, quoting |
| `internal/sequence` | the target value: direction, clamping, overflow |
| `internal/verify` | chunk boundaries, range queries, sampling, the report |
| `internal/cutover`, `internal/status`, `internal/format`, `internal/cli` | the lag arithmetic, the status output and JSON, number formatting, the command tree, the confirmation prompt |

### End-to-end tests

They run the real tool against PostgreSQL 16 (s1) and PostgreSQL 18 (s3) in Docker Compose ([test/e2e](../test/e2e)).

Prerequisites: a running Docker engine with Compose, Go, and the local ports 55416 and 55418 (both bound to 127.0.0.1). The images `postgres:16` and `postgres:18` are pulled on the first run.

```text
make e2e
go test -tags e2e -count=1 -timeout 30m ./test/e2e/...
```

The test builds the `linux/amd64` binary into `bin/`, starts fresh containers and removes them at the end. gophant runs inside the s3 container, which also carries the PostgreSQL 18 `pg_dump` and `pg_restore`. The password is random for every run, or taken from `GOPHANT_E2E_PASSWORD`; `GOPHANT_E2E_KEEP=1` keeps the containers for inspection. The containers can be started by hand with `GOPHANT_E2E_PASSWORD=... docker compose -f test/e2e/docker-compose.yml up -d --wait`.

`TestMigrationEndToEnd` (about one minute):

1. Seeds s1 ([testdata/seed.sql](../test/e2e/testdata/seed.sql)): an identity-always column, a generated column, a composite primary key, a table without primary key and with duplicate rows and json, a partitioned table, an array of a user-defined enum, TOASTed columns and a 300000 row table.
2. `precheck` passes.
3. A workload writes to s1 all the time (inserts, updates, deletes, partition moves, TOAST untouched or changed, rolled back transactions) and keeps writing through the whole test.
4. `snapshot` runs under that load; a second `snapshot` is refused.
5. `sync` loses its connection (`pg_terminate_backend` on the walsender) and reconnects by itself; then the process is killed with `SIGKILL` and started again.
6. The workload stops. A write transaction is left open: the cutover must refuse and the stage must stay `CUTOVER`. After the rollback the cutover completes, the sequences are `last_value + 100`, `sync` stops by itself.
7. A fingerprint of every table (row count and an MD5 of all rows) is equal on s1 and s3.
8. `verify` passes, a deliberately changed row makes it fail, after the repair it passes, `verify --drop-slot` drops the slot and the stage is `DONE`.

`TestSnapshotRecovery` (about twenty seconds): a table on s3 with the name of an index of s1 makes the post-data import fail; after removing it `snapshot` continues without copying any table; `snapshot --restart` without an answer is refused and changes nothing; `snapshot --restart --yes` starts over, including a row written in between, and ends equal to s1.

### Not covered

TLS connections, other PostgreSQL versions, large data volumes and throughput, a different encoding or collation between s1 and s3, partitioned tables with several levels, `snapshot.skip_ddl = true`, extensions, and tables that use a column type without a binary format.

---

# 中文

## 测试

### 单元测试

`go test ./...` 不需要数据库，也不需要 Docker。覆盖内容：

| Package | 覆盖内容 |
|---|---|
| `internal/config` | 默认值、`${NAME}` 展开、未知配置项、校验信息、表过滤 |
| `internal/pg` | 从连接串中拆出密码（URL 形式和键值形式），错误信息不泄露密码 |
| `internal/meta` | DDL、阶段取值、起始位点、带保护的 `applied_lsn` 语句 |
| `internal/catalog` | 哪些列类型可以安全地用二进制 COPY（数组、复合类型、domain、range、递归） |
| `internal/precheck` | 每条规则的判断、报告及其输出 |
| `internal/schema` | `pg_dump`/`pg_restore` 参数（密码从不作为参数）、pre-data 和 post-data 的恢复列表过滤 |
| `internal/snapshot` | COPY 语句、恶意标识符的引用、表的顺序、快照名检查 |
| `internal/cdc` | 逐字节构造并解码 `pgoutput` 消息、范围过滤、溢写文件（顺序、relation 版本、truncate、清理）、进度计数 |
| `internal/applier` | SQL 构建：普通表、未变更 TOAST、复合主键、改主键、无主键且含 `NULL` 和重复行、生成列和 identity 列、分区根表、文本比较、truncate、引用 |
| `internal/sequence` | 目标值：方向、边界限制、溢出 |
| `internal/verify` | 分片边界、区间查询、抽样、报告 |
| `internal/cutover`、`internal/status`、`internal/format`、`internal/cli` | lag 计算、状态输出和 JSON、数字格式、命令树、确认提示 |

### 端到端测试

它们在 Docker Compose 中用真实工具对 PostgreSQL 16（s1）和 PostgreSQL 18（s3）运行（[test/e2e](../test/e2e)）。

前置条件：运行中的 Docker 引擎（含 Compose）、Go，以及本机端口 55416 和 55418（都只绑定 127.0.0.1）。镜像 `postgres:16` 和 `postgres:18` 在第一次运行时拉取。

命令与英文版相同，见上面的代码块。

测试会把 `linux/amd64` 二进制构建到 `bin/`，启动全新的容器，并在结束时删除。gophant 在 s3 容器内运行，该容器同时带有 PostgreSQL 18 的 `pg_dump` 和 `pg_restore`。每次运行的密码是随机的，也可以用 `GOPHANT_E2E_PASSWORD` 指定；`GOPHANT_E2E_KEEP=1` 会保留容器以便排查。也可以手工启动容器：`GOPHANT_E2E_PASSWORD=... docker compose -f test/e2e/docker-compose.yml up -d --wait`。

`TestMigrationEndToEnd`（约一分钟）：

1. 向 s1 灌入测试数据（[testdata/seed.sql](../test/e2e/testdata/seed.sql)）：identity-always 列、生成列、复合主键、无主键且含重复行和 json 的表、分区表、用户自定义 enum 的数组、TOAST 列、一个 30 万行的表。
2. `precheck` 通过。
3. 一个负载持续向 s1 写入（插入、更新、删除、跨分区移动、TOAST 未变更或已变更、回滚的事务），贯穿整个测试。
4. 在该负载下运行 `snapshot`；第二次 `snapshot` 被拒绝。
5. `sync` 断开连接（对 walsender 执行 `pg_terminate_backend`）并自行重连；然后用 `SIGKILL` 杀掉进程并重新启动。
6. 负载停止。留下一个未提交的写事务：割接必须拒绝，阶段必须保持 `CUTOVER`。回滚之后割接完成，序列为 `last_value + 100`，`sync` 自行停止。
7. 每张表的指纹（行数和所有行的 MD5）在 s1 和 s3 上相等。
8. `verify` 通过；故意改动一行会让它失败；修复后再次通过；`verify --drop-slot` 删除槽，阶段为 `DONE`。

`TestSnapshotRecovery`（约二十秒）：s3 上有一张与 s1 某个索引同名的表，使 post-data 导入失败；删除它之后，`snapshot` 在不拷贝任何表的情况下继续；没有回答的 `snapshot --restart` 被拒绝且不改变任何东西；`snapshot --restart --yes` 从头开始，包括期间写入的一行，最终与 s1 相等。

### 未覆盖

TLS 连接、其他 PostgreSQL 版本、大数据量和吞吐、s1 与 s3 编码或排序规则不同、多级分区表、`snapshot.skip_ddl = true`、扩展，以及使用没有二进制格式的列类型的表。
