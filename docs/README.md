# English

## Documentation index

Look a module up here before changing or debugging it, and add every new document to this index.

| Document | Describes |
|---|---|
| [gophant.md](gophant.md) | The original design specification (Chinese). The architecture, the five stages, the `gophant_meta` tables and the milestones. |
| [design_deviations.md](design_deviations.md) | Where the implementation differs from, or makes precise, the design specification, and why. Read this first if you compare the code with `gophant.md`. |
| [operations.md](operations.md) | The runbook: prerequisites on s1 and s3, the commands in order, the configuration reference, recovery. |
| [precheck.md](precheck.md) | The `precheck` stage: every rule, its severity, and the lessons learned. |
| [snapshot.md](snapshot.md) | The `snapshot` stage: slot and exported snapshot, schema import, the parallel copy, restart and resume. |
| [cdc_applier.md](cdc_applier.md) | The `sync` stage: stream reader, transaction buffer, applier, exactly-once, positions and heartbeat. |
| [cutover_verify.md](cutover_verify.md) | The `cutover`, `verify` and `status` stages: lag zero, sequences, verification. |
| [testing.md](testing.md) | Unit tests and the Docker Compose end-to-end tests. |

Documents are bilingual: the complete English version comes first, the complete Simplified Chinese version second.

---

# 中文

## 文档索引

修改或排查某个 module 之前先在此查找，新增文档时也要加入本索引。

| 文档 | 内容 |
|---|---|
| [gophant.md](gophant.md) | 原始设计方案（中文）：架构、五个阶段、`gophant_meta` 表和里程碑。 |
| [design_deviations.md](design_deviations.md) | 实现与设计方案不同或更精确的地方及原因。对照 `gophant.md` 阅读代码时请先看这份。 |
| [operations.md](operations.md) | 运行手册：s1、s3 的前置条件、按顺序执行的命令、配置参考、故障恢复。 |
| [precheck.md](precheck.md) | `precheck` 阶段：每条规则、严重级别和经验教训。 |
| [snapshot.md](snapshot.md) | `snapshot` 阶段：复制槽与导出快照、表结构导入、并发拷贝、重启与续跑。 |
| [cdc_applier.md](cdc_applier.md) | `sync` 阶段：流读取、事务缓冲、回放器、exactly-once、位点与心跳。 |
| [cutover_verify.md](cutover_verify.md) | `cutover`、`verify`、`status` 阶段：lag 归零、序列、校验。 |
| [testing.md](testing.md) | 单元测试和基于 Docker Compose 的端到端测试。 |

文档为双语：先是完整的英文版，再是完整的简体中文版。
