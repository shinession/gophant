# English

## gophant

gophant migrates a PostgreSQL 16 database (s1) to PostgreSQL 18 (s3) with a short cutover window and no lost data. It copies a consistent snapshot with parallel binary `COPY`, replays the logical WAL stream (`pgoutput`) exactly once until the cutover, and verifies the result.

The five stages are one command each:

```text
gophant precheck  -c gophant.yaml    # check s1 and s3
gophant snapshot  -c gophant.yaml    # schema, slot, parallel copy
gophant sync      -c gophant.yaml    # replay the WAL stream
gophant cutover   -c gophant.yaml    # lag 0, sequences (writes to s1 stopped first)
gophant verify    -c gophant.yaml    # row counts and chunk hashes, --drop-slot to finish
gophant status    -c gophant.yaml    # stage, lag, progress at any time
```

Build: `make build` (Go and GNU make; the binary has no CGo dependency). Test: `make test`, and `make e2e` with Docker.

Start with the runbook [docs/operations.md](docs/operations.md). The index of all documents is [docs/README.md](docs/README.md); the design is [docs/gophant.md](docs/gophant.md) (Chinese) and [docs/design_deviations.md](docs/design_deviations.md) lists where the code differs from it. The example configuration is [configs/gophant.example.yaml](configs/gophant.example.yaml). The status of the work is in [plan.md](plan.md).

---

# 中文

## gophant

gophant 把 PostgreSQL 16 数据库（s1）迁移到 PostgreSQL 18（s3），割接窗口很短，数据不丢。它用并发的二进制 `COPY` 拷贝一致性快照，exactly-once 地回放逻辑 WAL 流（`pgoutput`）直到割接，并校验结果。

五个阶段各对应一条命令，命令与英文版相同，见上面的代码块。

构建：`make build`（需要 Go 和 GNU make；二进制不依赖 CGo）。测试：`make test`，有 Docker 时用 `make e2e`。

请从运行手册 [docs/operations.md](docs/operations.md) 开始。所有文档的索引是 [docs/README.md](docs/README.md)；设计是 [docs/gophant.md](docs/gophant.md)（中文），[docs/design_deviations.md](docs/design_deviations.md) 列出了代码与它的差异。配置模板是 [configs/gophant.example.yaml](configs/gophant.example.yaml)。工作状态见 [plan.md](plan.md)。
