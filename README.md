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
gophant run       -c gophant.yaml    # many databases: precheck, snapshot and sync of all of them
```

A cluster with many databases needs no configuration per database: with `databases.auto: true` gophant finds the databases of s1, checks them together, creates what is missing (the databases on s3, the publications on s1 when the role may), and every command works on all of them (`--database <name>` limits it). See [docs/multi_database.md](docs/multi_database.md).

Build: `make build` (Go and GNU make; the binary has no CGo dependency). Test: `make test`, and `make e2e` with Docker.

Release packages (Windows PowerShell or PowerShell 7, no make needed):

```text
powershell -NoProfile -ExecutionPolicy Bypass -File .\release.ps1 -Version v0.1.0
```

It runs `go vet` and `go test`, builds `linux/amd64` and `windows/amd64` (change it with `-Targets`, for example `-Targets linux/amd64,linux/arm64`) and writes to `release/<version>/` one archive per platform (`.tar.gz` for Linux, with the binary already executable, `.zip` for Windows), each holding the binary, `configs/gophant.example.yaml`, `README.md` and `docs/`, plus `SHA256SUMS.txt` (`sha256sum -c SHA256SUMS.txt`). `-SkipTests` skips the tests. Without `-Version` the name comes from `git describe`, so tag the commit first to get a clean name. Nothing is uploaded. On the server: `tar -xzf gophant_<version>_linux_amd64.tar.gz` and run `./gophant --version`.

Start with the runbook [docs/operations.md](docs/operations.md). The index of all documents is [docs/README.md](docs/README.md); the design is [docs/gophant.md](docs/gophant.md) (Chinese) and [docs/design_deviations.md](docs/design_deviations.md) lists where the code differs from it. The example configuration is [configs/gophant.example.yaml](configs/gophant.example.yaml). The status of the work is in [plan.md](plan.md).

---

# 中文

## gophant

gophant 把 PostgreSQL 16 数据库（s1）迁移到 PostgreSQL 18（s3），割接窗口很短，数据不丢。它用并发的二进制 `COPY` 拷贝一致性快照，exactly-once 地回放逻辑 WAL 流（`pgoutput`）直到割接，并校验结果。

五个阶段各对应一条命令，命令与英文版相同，见上面的代码块。

有很多数据库的集群不需要每个库一份配置：设置 `databases.auto: true` 后，gophant 会发现 s1 上的数据库，把它们放在一起检查，创建缺少的东西（s3 上的库；角色有权限时还有 s1 上的 publication），所有命令都对全部库工作（`--database <name>` 可限定范围）。见 [docs/multi_database.md](docs/multi_database.md)。

构建：`make build`（需要 Go 和 GNU make；二进制不依赖 CGo）。测试：`make test`，有 Docker 时用 `make e2e`。

打包发布（Windows PowerShell 或 PowerShell 7，不需要 make）：命令与英文版相同，见上面的代码块。

它会运行 `go vet` 和 `go test`，构建 `linux/amd64` 和 `windows/amd64`（可用 `-Targets` 修改，例如 `-Targets linux/amd64,linux/arm64`），并在 `release/<version>/` 下为每个平台生成一个压缩包（Linux 为 `.tar.gz`，其中的二进制已带可执行权限；Windows 为 `.zip`），包内有二进制、`configs/gophant.example.yaml`、`README.md` 和 `docs/`，另有 `SHA256SUMS.txt`（可用 `sha256sum -c SHA256SUMS.txt` 校验）。`-SkipTests` 跳过测试。不带 `-Version` 时版本名取自 `git describe`，所以想要干净的名字请先给提交打 tag。脚本不会上传任何东西。在服务器上：`tar -xzf gophant_<version>_linux_amd64.tar.gz`，然后运行 `./gophant --version`。

请从运行手册 [docs/operations.md](docs/operations.md) 开始。所有文档的索引是 [docs/README.md](docs/README.md)；设计是 [docs/gophant.md](docs/gophant.md)（中文），[docs/design_deviations.md](docs/design_deviations.md) 列出了代码与它的差异。配置模板是 [configs/gophant.example.yaml](configs/gophant.example.yaml)。工作状态见 [plan.md](plan.md)。
