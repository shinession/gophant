# gophant — PostgreSQL 16 (s1) → PostgreSQL 18 (s3) 零丢数据增量迁移工具设计方案

> **文档用途**：本文件作为 `gophant` 项目的核心架构说明书与 AI Coding Agent 实施指南。Agent 应严格遵循本文档的模块划分、数据流协议、一致性约束与避坑红线进行项目骨架搭建与分阶段编码。

## 1. 项目背景与核心目标

* **源库 (`s1`)**：PostgreSQL 16（不支持原生增量物理备份）。

* **目标库 (`s3`)**：PostgreSQL 18。

* **核心约束**：生产环境最终可接受的**停机割接窗口** $\le$ **30 分钟**；割接前 `s1` 允许重启一次以启用 `wal_level = logical`。

* **技术路线（方案 A）**：采用 **Go 并发二进制 `COPY` 全量快照 + PostgreSQL 原生 WAL 逻辑解码（`pgoutput`）增量追平**。

* **核心保障**：

  1. **全量与增量严格咬合**：借助逻辑复制槽导出的 `EXPORT_SNAPSHOT` 与 `consistent_lsn`，保证全量与增量之间不丢、不重。

  2. **Exactly-Once 幂等回放**：在 `s3` 上将业务 DML 回放与 `applied_lsn` 位点更新放入**同一个数据库事务**，实现崩溃后精准断点续传。

  3. **生产安全保护**：强制校验 `s1` 的 `max_slot_wal_keep_size`，防止 WAL 积压撑爆生产磁盘。

## 2. 核心架构与数据流图

### 2.1 系统拓扑与通道架构

架构包含 5 个核心实体与 3 类通信流（全量/WAL 数据流、LSN 心跳反馈流、控制/校验流）：

```
flowchart LR
    %% 节点定义（对应架构交互图）
    S1(["s1 源库 · PG 16<br/>logical slot + WAL"])
    S3(["s3 目标库 · PG 18<br/>业务表 + gophant_meta"])
    SNAP(["Snapshotter<br/>全量 COPY 管道"])
    CDC(["CDC Reader<br/>pgoutput WAL 解码"])
    APP(["Applier<br/>幂等批量回放器"])

    %% 1. 全量拷贝通道
    S1 ==>|COPY| SNAP
    SNAP ==>|BINARY| S3

    %% 2. 控制 / 预检 / 校验通道
    S1 -.->|DDL / 预检 / 校验| S3

    %% 3. WAL 实时增量通道
    S1 ==>|WAL| CDC
    CDC ==>|TXN| APP
    APP ==>|Batch| S3

    %% 4. 心跳与 WAL 回收通道
    APP -.->|LSN 心跳反馈| S1

    %% 样式区分
    classDef db fill:#162447,stroke:#5b8ff9,stroke-width:2px,color:#fff;
    classDef worker fill:#1f2937,stroke:#6b7280,stroke-width:1.5px,color:#e5e7eb;
    class S1,S3 db;
    class SNAP,CDC,APP worker;

```

### 2.2 五阶段执行状态机

```
stateDiagram-v2
    direction LR
    [*] --> PRECHECK: 1. 预检建槽
    PRECHECK --> SNAPSHOT: 2. 全量拷贝
    SNAPSHOT --> CDC_STREAM: 3. WAL 追平
    CDC_STREAM --> CUTOVER: 4. 停机割接
    CUTOVER --> VERIFY: 5. 校验切流
    VERIFY --> [*]

```

| 阶段 | 阶段名称 | 活跃组件 | 激活通道 | 核心动作与达成条件 | 
| ----- | ----- | ----- | ----- | ----- | 
| **阶段 1** | `PRECHECK`  预检建槽 | `s1`, `s3` | `DDL / 预检` | 检查 `s1` 参数、权限、无主键表、UNLOGGED 表、PG 18 兼容性；导入 `pre-data` 表结构到 `s3`；初始化 `gophant_meta`；创建 `gophant_slot` 获取 `snapshot_name` 与 `consistent_lsn`。 | 
| **阶段 2** | `SNAPSHOT`  全量拷贝 | `s1`, `Snapshotter`, `s3` | `COPY` → `BINARY` | 保持建槽连接存活，多 Worker 共享同一 `snapshot_name` 并发流式 `COPY BINARY`（零落盘）；拷贝结束后在 `s3` 导入 `post-data`（索引、外键、约束）。 | 
| **阶段 3** | `CDC_STREAM`  WAL 追平 | `s1`, `CDC Reader`, `Applier`, `s3` | `WAL` → `TXN` → `Batch`  `LSN 心跳反馈` | 从 `consistent_lsn`（或断点 `applied_lsn`）消费 `pgoutput` 流；按事务组装提交；`s3` 端同一事务写入业务数据与 `applied_lsn`；提交后发送 `StandbyStatusUpdate` 推进 `s1` 回收 WAL。 | 
| **阶段 4** | `CUTOVER`  停机割接 | `s1`, `CDC Reader`, `Applier`, `s3` | `WAL` → `Batch`  `DDL / 预检` | **进入 30 分钟停机窗口**：`s1` 停写 → 等待 `lag = 0`（`pg_current_wal_lsn() == applied_lsn`）→ 全库 Sequence `last_value` 同步至 `s3`（`setval`）。 | 
| **阶段 5** | `VERIFY`  校验切流 | `s1`, `s3` | `DDL / 预检 / 校验` | 执行行数对比与主键分片哈希校验；校验通过后业务切流至 `s3`；清理 `s1` 的 `gophant_slot` 释放 WAL 保留。 | 

## 3. 技术栈选型（极简、零 CGo）

* **Go 版本**：`Go 1.23+`

* **PostgreSQL 驱动与连接池**：`github.com/jackc/pgx/v5`（标准连接、Batch、`COPY` 管道）

* **逻辑复制协议**：`github.com/jackc/pglogrepl`（配合 `pgx/v5/pgconn` 处理 `CREATE_REPLICATION_SLOT`、`START_REPLICATION`、`pgoutput` 解码与 `StandbyStatusUpdate`）

* **CLI 框架**：`github.com/spf13/cobra`

* **配置解析**：`gopkg.in/yaml.v3`

* **并发协调**：`golang.org/x/sync/errgroup`

* **日志系统**：Go 标准库 `log/slog`（JSON 结构化日志）

* **外部二进制依赖**：`s3` 端（PG 18）自带的 `pg_dump` 与 `pg_restore`（高版本 `pg_dump` 向下兼容 PG 16，用于拆解 `pre-data` / `post-data` DDL）

## 4. 项目目录结构骨架

Agent 生成骨架时应按以下目录结构组织代码：

```
gophant/
├── cmd/
│   └── gophant/
│       └── main.go                 # CLI 入口，注册子命令
├── configs/
│   └── gophant.example.yaml        # 配置文件模板
├── internal/
│   ├── cli/                        # cobra 命令定义 (precheck/snapshot/sync/cutover/verify/status)
│   ├── config/
│   │   └── config.go               # YAML 配置结构体、默认值与校验
│   ├── meta/
│   │   ├── schema.go               # s3 端 gophant_meta 初始化 DDL
│   │   └── store.go                # sync_state 与 table_progress 读写（支持传入 pgx.Tx）
│   ├── precheck/
│   │   ├── checker.go              # 预检编排与报告输出
│   │   └── rules.go                # 参数、权限、无主键、UNLOGGED、生成列、扩展、MD5 检查项
│   ├── schema/
│   │   └── ddl.go                  # 封装 PG 18 pg_dump/pg_restore 执行 pre-data / post-data
│   ├── snapshot/
│   │   ├── slot.go                 # 复制槽创建与 EXPORT_SNAPSHOT 生命周期保活
│   │   └── copier.go               # 多 Worker 并发流式 COPY BINARY 管道（io.Pipe）
│   ├── cdc/
│   │   ├── reader.go               # pglogrepl 复制流消费循环、心跳定时触发
│   │   ├── decoder.go              # pgoutput 协议解码、RelationMap 维护
│   │   └── txbuffer.go             # 事务组装缓存（内存 + 大事务溢写本地临时文件防 OOM）
│   ├── applier/
│   │   ├── applier.go              # s3 连接初始化（replica role）、pgx.Batch 事务回放
│   │   └── sqlbuilder.go           # INSERT/UPDATE/DELETE/TRUNCATE 语句构建（处理 TOAST/无PK）
│   ├── sequence/
│   │   └── sync.go                 # s1 pg_sequences 拉取与 s3 setval() 同步
│   └── verify/
│       ├── count.go                # 全表精确行数对账
│       └── checksum.go             # 按主键区间分块 MD5/XOR 抽样或全量校验
├── go.mod
├── go.sum
├── Makefile
└── plan.md

```

## 5. `s3` 元数据表设计（`gophant_meta`）

所有状态直接持久化在 `s3` 独立 Schema 中，无需引入外部状态存储：

```
CREATE SCHEMA IF NOT EXISTS gophant_meta;

-- 全局迁移状态与位点表（单行记录，id 恒为 1）
CREATE TABLE IF NOT EXISTS gophant_meta.sync_state (
    id              INT PRIMARY KEY CHECK (id = 1),
    stage           TEXT NOT NULL,          -- PRECHECK | SNAPSHOT | CDC_STREAM | CUTOVER | VERIFY | DONE
    slot_name       TEXT NOT NULL,
    publication     TEXT NOT NULL,
    consistent_lsn  PG_LSN,                 -- 建槽时返回的一致性快照位点
    applied_lsn     PG_LSN,                 -- 已在 s3 事务中原子落库的最新 LSN
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 表级全量拷贝与校验进度表
CREATE TABLE IF NOT EXISTS gophant_meta.table_progress (
    schema_name     TEXT NOT NULL,
    table_name      TEXT NOT NULL,
    has_pk          BOOLEAN NOT NULL DEFAULT true,
    replica_id      CHAR(1) NOT NULL DEFAULT 'd', -- d: default, f: full, i: index, n: nothing
    snapshot_status TEXT NOT NULL DEFAULT 'PENDING', -- PENDING | RUNNING | DONE | FAILED
    rows_copied     BIGINT NOT NULL DEFAULT 0,
    verified_ok     BOOLEAN,
    error_msg       TEXT,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (schema_name, table_name)
);

```

## 6. 核心模块实现规范与避坑红线

Agent 在实现以下核心逻辑时，**必须满足全部约束**：

### 6.1 `precheck`（迁移前体检）

启动迁移前自动检查并输出 PASS / WARN / FAIL 报告：

1. **必过红线（FAIL 则阻断）**：

   * `s1` `wal_level == 'logical'`；

   * `s1` 当前账号具备 `REPLICATION` 与全表读取权限；

   * `s1` 已配置 `max_slot_wal_keep_size > 0`（防止槽位积压打满生产盘，除非配置显式允许忽略）；

   * 目标 `s3` 已安装 `s1` 中所有业务用到的扩展插件（`pg_extension` 对齐）。

2. **风险识别（WARN 或阻断）**：

   * **无主键表检查**：扫描缺失主键且 `relreplident != 'f'`（非 `FULL`）的表，提示需补充主键或执行 `ALTER TABLE ... REPLICA IDENTITY FULL;`；

   * **UNLOGGED 表检查**：列出所有 `relpersistence = 'u'` 的表（逻辑解码不追踪 UNLOGGED 表，且 PG 18 禁止 UNLOGGED 分区表）；

   * **生成列检查**：标记 `information_schema.columns` 中 `is_generated = 'ALWAYS'` 的列；

   * **认证方式检查**：提示 `pg_authid` 中仍使用 `md5` 的角色（PG 18 已弃用 MD5）。

### 6.2 `snapshot`（零落盘并发全量快照）

1. **DDL 拆分**：

   * 先用 PG 18 的 `pg_dump --schema-only --section=pre-data` 导出表结构并导入 `s3`；

   * 全量数据灌完后，再执行 `pg_dump --schema-only --section=post-data` 在 `s3` 端重建索引、外键、约束与触发器（大幅提升全量导入吞吐）。

2. **快照导出与保活**：

   * 使用 Replication 连接执行 `CREATE_REPLICATION_SLOT <slot_name> LOGICAL pgoutput EXPORT_SNAPSHOT`（在事务块内），保存返回的 `consistent_lsn` 和 `snapshot_name`。

   * **关键约束**：该控制连接在所有表的全量拷贝完成前**绝不能关闭或提交事务**，否则 `snapshot_name` 会立即失效。

3. **流式并发 `COPY`**：

   * 启动 `N` 个 Worker（由 `snapshot.workers` 配置），每个 Worker 打开一对 `s1` / `s3` 连接；

   * `s1` 端执行：`BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ; SET TRANSACTION SNAPSHOT '<snapshot_name>';`

   * `s3` 端执行：`SET session_replication_role = 'replica';`

   * 显式构造排除生成列（`is_generated = 'ALWAYS'`）的字段列表，通过 `io.Pipe()` 将 `s1` 的 `COPY schema.table (cols...) TO STDOUT (FORMAT BINARY)` 直接零落盘流式喂给 `s3` 的 `COPY schema.table (cols...) FROM STDIN (FORMAT BINARY)`。

### 6.3 `cdc`（WAL 解码与事务缓冲）

1. **启动位点**：

   * 优先读取 `s3` 的 `gophant_meta.sync_state.applied_lsn`；若为空，则使用 `consistent_lsn`。

   * 调用 `pglogrepl.StartReplication`（`proto_version '1'`, `publication_names '<pub>'`）。

2. **事务组装与防 OOM**：

   * 缓存 `BeginMessage` 至 `CommitMessage` 之间的所有变更事件（`Insert` / `Update` / `Delete` / `Truncate`）；

   * 仅当收到 `CommitMessage` 且 `CommitLSN > applied_lsn` 时才投递给 `Applier`，自动跳过未提交或已回滚的事务；

   * 当单事务缓冲体积超过 `cdc.max_tx_memory_mb`（如 64MB）时，自动溢写至本地临时文件流式读取，防止大事务导致 OOM。

### 6.4 `applier`（s3 幂等回放与 5 大暗坑处理）

1. **会话初始化**：每次建立 `s3` 回放连接必须执行 `SET session_replication_role = 'replica';`，避免业务触发器二次触发和外键检查干扰。

2. **暗坑 1 — `UnchangedTOAST` 处理（必须严格实现）**：

   * 解码 `UpdateMessage` 时，检查每列的 `TupleDataColumn.DataType`；

   * 当 `DataType == 'u'`（`pglogrepl.TupleDataTypeToast`）时，**绝对不能把该列写入 `UPDATE ... SET` 子句中**，必须跳过以保留 `s3` 原有 TOAST 值。

3. **暗坑 2 — 生成列过滤**：

   * 根据表元数据过滤掉 `is_generated = 'ALWAYS'` 的列，不参与 `INSERT`/`UPDATE` 列清单。

4. **暗坑 3 — 无主键表（`REPLICA IDENTITY FULL`）**：

   * `UPDATE` / `DELETE` 定位条件若无主键，使用 `OldTuple` 全部列构建 `WHERE` 条件（注意 `NULL` 必须使用 `IS NULL` 而非 `= NULL`），并通过 `WHERE ctid = (SELECT ctid FROM schema.table WHERE ... LIMIT 1)` 限制单条操作仅命中 1 行，避免重复行被误删/误改。

5. **暗坑 4 — Exactly-Once 原子位点更新与心跳分离**：

   * 将当前事务（或合并微批）的所有 DML 放入 `pgx.Batch`，在**同一个 `s3` 事务末尾**执行： `UPDATE gophant_meta.sync_state SET applied_lsn = $1, updated_at = now() WHERE id = 1;`

   * **只有 `s3` 事务 `Commit()` 成功后**，才更新内存中已确认的 `flushLSN`，由心跳协程定期向 `s1` 发送 `StandbyStatusUpdate` 通知推进 WAL 回收。

6. **暗坑 5 — 分区表路由**：

   * `s1` 端 Publication 强制开启 `publish_via_partition_root = true`，回放统一走父表路由。

### 6.5 `sequence` 与 `verify`（割接与对账）

1. **Lag 归零检测**：割接时轮询 `s1` 的 `pg_current_wal_lsn()` 与 `s3` 的 `gophant_meta.sync_state.applied_lsn`，确认差值为 0 字节且持续稳定。

2. **序列同步**：从 `s1` 查询 `pg_sequences`（`schemaname`, `sequencename`, `last_value`），在 `s3` 执行： `SELECT setval(format('%I.%I', $1, $2), $3 + $offset, true);`（仅同步 `last_value IS NOT NULL` 的序列，默认安全偏移 `offset = 100`）。

3. **一致性校验**：

   * 第一层：并发比对 `s1` 与 `s3` 全表 `COUNT(*)`；

   * 第二层：对含主键的表按主键区间切分 Chunk，计算列拼接后的聚合哈希比对（支持全量或按比例抽样，确保能在停机半小时内完成）。

## 7. CLI 命令与配置文件规范

### 7.1 CLI 子命令设计

```
gophant precheck -c gophant.yaml               # 阶段 1a：环境与兼容性体检
gophant snapshot -c gophant.yaml               # 阶段 1b & 2：导入 pre-data、建槽、并发全量拷贝、导入 post-data
gophant sync     -c gophant.yaml               # 阶段 3：持续消费 WAL 实时追平（支持断点续传与优雅退出）
gophant cutover  -c gophant.yaml               # 阶段 4：等待 lag=0、同步所有 Sequence、更新状态
gophant verify   -c gophant.yaml               # 阶段 5：行数与哈希校验（可选 --drop-slot 清理 s1 复制槽）
gophant status   -c gophant.yaml               # 查看当前阶段、LSN 延迟字节数、各表拷贝进度

```

### 7.2 配置文件模板（`configs/gophant.example.yaml`）

```
source:
  dsn: "postgres://gophant_repl:password@s1-host:5432/appdb?sslmode=prefer"
  replication_dsn: "postgres://gophant_repl:password@s1-host:5432/appdb?replication=database"
  slot_name: "gophant_slot"
  publication_name: "gophant_pub"

target:
  dsn: "postgres://postgres:password@s3-host:5432/appdb?sslmode=prefer"
  meta_schema: "gophant_meta"

tools:
  pg_dump_bin: "/usr/lib/postgresql/18/bin/pg_dump"
  pg_restore_bin: "/usr/lib/postgresql/18/bin/pg_restore"

filter:
  include_schemas: ["public"]
  exclude_tables: []

snapshot:
  workers: 4
  skip_ddl: false

cdc:
  standby_status_interval: "5s"
  batch_max_txns: 50
  batch_max_delay: "200ms"
  max_tx_memory_mb: 64
  temp_dir: "/tmp/gophant-spill"

cutover:
  sequence_offset: 100
  lag_zero_timeout: "5m"

verify:
  workers: 4
  chunk_size: 50000
  mode: "count_and_sample" # count_only | count_and_sample | full_hash
  sample_ratio: 0.1

```

## 8. `s1`（PG 16）前置环境配置清单（供人工 DBA 操作）

1. **`postgresql.conf`（需重启一次 `s1` 生效）**：

   ```
   wal_level = logical
   max_replication_slots = 8
   max_wal_senders = 8
   
   ```

2. **`postgresql.conf`（`SELECT pg_reload_conf();` 热加载生效）**：

   ```
   max_slot_wal_keep_size = '30GB'   # 必设保命参数，按 s1 剩余磁盘 30%~50% 调整
   wal_sender_timeout = '5min'
   
   ```

3. **专用复制账号与 `pg_hba.conf`**：

   ```
   CREATE ROLE gophant_repl WITH LOGIN REPLICATION PASSWORD '强密码';
   GRANT pg_read_all_data TO gophant_repl;
   CREATE PUBLICATION gophant_pub FOR ALL TABLES WITH (publish_via_partition_root = true);
   
   ```

   ```
   host    appdb           gophant_repl    <gophant_ip>/32    scram-sha-256
   host    replication     gophant_repl    <gophant_ip>/32    scram-sha-256
   
   ```

4. **同步期间约束**：自 `snapshot` 启动至 `cutover` 结束，冻结 `s1` 的 DDL 变更。

## 9. AI Coding Agent 分阶段实施里程碑（Milestones）

Agent 应按以下顺序逐步交付，每个里程碑完成后执行 `go test ./...` 与 `go build ./cmd/gophant` 确保可编译通过：

* **Milestone 1：项目骨架与基础底座**

  * 初始化 `go.mod` 与目录结构、`Makefile`、`configs/gophant.example.yaml`。

  * 实现 `internal/config`、`internal/cli` 全部子命令桩函数、`log/slog` 初始化与信号优雅退出（`signal.NotifyContext`）。

  * 实现 `internal/meta`（`gophant_meta` 建表及事务内更新 `applied_lsn` 方法）。

* **Milestone 2：预检器与 DDL 同步器**

  * 实现 `internal/precheck` 全部规则及结构化体检报告打印。

  * 实现 `internal/schema` 调用 PG 18 `pg_dump` / `pg_restore` 执行 `pre-data` 与 `post-data`。

* **Milestone 3：一致性全量快照管道**

  * 实现 `internal/snapshot/slot.go`（建槽 + `EXPORT_SNAPSHOT` 保活）。

  * 实现 `internal/snapshot/copier.go`（排除生成列、多 Worker `io.Pipe` 流式 `COPY BINARY` + 进度落库）。

* **Milestone 4：WAL 增量解码与幂等回放引擎（核心）**

  * 实现 `internal/cdc`（`pglogrepl` 消费循环、`pgoutput` 关系缓存、`txbuffer` 内存与磁盘溢写）。

  * 实现 `internal/applier`（`replica` 会话角色、跳过 `UnchangedTOAST`、无主键 `ctid` 保护、同一事务更新 `applied_lsn`、提交后回报 `StandbyStatusUpdate`）。

  * 编写针对 `sqlbuilder` 的单元测试（覆盖普通表、TOAST 未变更列、复合主键、无主键 NULL 值、生成列）。

* **Milestone 5：割接、序列同步与数据校验**

  * 实现 `internal/sequence`（`pg_sequences` 读取与 `setval`）。

  * 实现 `internal/verify`（行数对账 + 分片哈希）与 `gophant status` 监控面板输出。

  * 编写基于 Docker Compose（PG 16 + PG 18）的端到端集成测试脚本，验证全量拷贝期间并发写入 + 断线重连断点续传 + 停机割接全流程。