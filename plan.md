# English

## Implementation plan and status

This is the milestone list of [docs/gophant.md](docs/gophant.md) section 9 with its current state. The state is what has been built and run, not what is intended.

| Milestone | Content | State |
|---|---|---|
| 1. Skeleton and foundation | `go.mod`, layout, `Makefile`, example configuration, `internal/config`, all commands, `log/slog`, graceful shutdown on SIGINT/SIGTERM, `internal/meta` with the transactional update of `applied_lsn` | Done. Unit tests pass. |
| 2. Precheck and DDL | `internal/precheck` with every rule and the report, `internal/schema` with `pg_dump`/`pg_restore` for pre-data and post-data | Done. Unit tests pass; run against PostgreSQL 16 and 18 in the end-to-end tests. |
| 3. Consistent snapshot | `internal/snapshot` slot with exported snapshot, parallel streaming `COPY`, progress in `gophant_meta`, restart and resume | Done. Run under write load in the end-to-end tests. |
| 4. WAL replay | `internal/cdc` (decoder, buffer with spill, reader, heartbeat), `internal/applier` (SQL builder and its unit tests, replay, exactly-once), `internal/syncer` | Done. Unit tests pass; interrupted by a dropped connection and by `SIGKILL` in the end-to-end test. |
| 5. Cutover and verification | `internal/sequence`, `internal/verify`, `gophant status`, `internal/cutover`, the Docker Compose end-to-end tests | Done. The end-to-end tests pass. |

Where the implementation differs from the design, and why, is listed in [docs/design_deviations.md](docs/design_deviations.md). The runbook is [docs/operations.md](docs/operations.md).

### Not done and not verified

* No throughput measurement on a large database: the tests use a few hundred thousand rows.
* One table is copied by one stream; a huge table that is not partitioned is not split.
* Not tested: TLS connections, PostgreSQL versions other than 16 to 18, multi-level partitioning, `snapshot.skip_ddl`, extensions, different encodings or collations.
* Large objects are not migrated.

---

# 中文

## 实施计划与状态

这是 [docs/gophant.md](docs/gophant.md) 第 9 节的里程碑清单及其当前状态。状态指的是已经构建并运行过的内容，而不是计划。

| 里程碑 | 内容 | 状态 |
|---|---|---|
| 1. 骨架与基础底座 | `go.mod`、目录结构、`Makefile`、配置模板、`internal/config`、全部命令、`log/slog`、SIGINT/SIGTERM 优雅退出、`internal/meta` 及事务内更新 `applied_lsn` | 已完成。单元测试通过。 |
| 2. 预检器与 DDL | `internal/precheck` 的全部规则和报告、`internal/schema` 用 `pg_dump`/`pg_restore` 处理 pre-data 和 post-data | 已完成。单元测试通过；在端到端测试中对 PostgreSQL 16 和 18 运行过。 |
| 3. 一致性快照 | `internal/snapshot` 的导出快照建槽、并发流式 `COPY`、`gophant_meta` 中的进度、重启与续跑 | 已完成。在端到端测试中于写入负载下运行过。 |
| 4. WAL 回放 | `internal/cdc`（解码器、带溢写的缓冲、reader、心跳）、`internal/applier`（SQL 构建及其单元测试、回放、exactly-once）、`internal/syncer` | 已完成。单元测试通过；在端到端测试中被断连和 `SIGKILL` 打断过。 |
| 5. 割接与校验 | `internal/sequence`、`internal/verify`、`gophant status`、`internal/cutover`、Docker Compose 端到端测试 | 已完成。端到端测试通过。 |

实现与设计的差异及原因见 [docs/design_deviations.md](docs/design_deviations.md)。运行手册是 [docs/operations.md](docs/operations.md)。

### 未做与未验证

* 没有在大型数据库上测量吞吐：测试只用了几十万行。
* 一张表由一个流拷贝；没有分区的超大表不会被拆分。
* 未测试：TLS 连接、16 到 18 之外的 PostgreSQL 版本、多级分区、`snapshot.skip_ddl`、扩展、不同的编码或排序规则。
* 大对象不会被迁移。
