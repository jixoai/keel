# Proposal: fix-windows-pg-start

## Why

v0.6.0 的 Windows 实机验收发现 P0：`keel.exe` 无参首次启动卡死。内嵌 PostgreSQL
18 已初始化并监听 `127.0.0.1:13741`，但 `keel` 永不监听 `127.0.0.1:13750`；
`keel db start` 在 20s 后超时（exit 124），而 PG 已启动。`keel db dsn/status` 与
`keel db stop` 均正常。

根因：`startDevDBCluster` 用 `exec.Command(...).CombinedOutput()` 启动
`pg_ctl -w start`。Windows 下 `pg_ctl` 启动的 postgres 守护进程继承了 stdout/stderr
管道写端；`CombinedOutput` 等待管道 EOF，而 postmaster 一直持有写端，于是永久阻塞。
Ubuntu CI 不覆盖 Windows 的句柄继承行为，v0.6.0 因此只在 Windows 上暴露。

证据（Windows 11 家庭版中文版 10.0.26300，AMD64）：
`evidence/` 中的 hang 诊断与 `keel db stop/start` 复现；详见
`keel-windows-verification` 的 `04a-start-hang-diagnosis.txt` 与
`04c-db-start-repro.txt`。

## What Changes

- `startDevDBCluster` 不再用 `CombinedOutput()`；新增 `startPGDaemon`：
  - 把 `pg_ctl` 的 stdout/stderr 写到 `<dev-pg>.log.ctl`（普通文件，不是管道）；
  - 用 `Run()` 只等待 `pg_ctl` 自身退出，不等待 postmaster 的 stdio；
  - 失败时返回错误，并带 ctl log 与 PG log 路径，便于排障。
- 新增跨平台回归测试：fake `pg_ctl` 启动一个继承 stdio、存活 5s 的子进程后立即退出；
  `startPGDaemon` 必须在 4s 内返回并写出 ctl log。旧实现会等 5s，测试失败。
- 版本 bump 到 `0.6.1`；按 Keel release workflow 发布 `v0.6.1`，不改 `v0.6.0`
  tag 或已发布资产。
- 在 `docs/RUNBOOK.md` 的 Windows 验收/排障段落记录该缺陷与修复，明确“PG ready”
  不等于“keel start 返回”。
- 独立复审（子代理）后归档；v0.6.1 Windows 资产在真实 Windows 上重跑
  `keel-windows-verification` 的启动、探针与持久化验收。

## Impact

- 代码：`cmd/keel/db.go`（start 路径与 helper）、`cmd/keel/db_test.go`（回归测试）、
  `cmd/keel/main.go`（version）。
- 文档：`docs/RUNBOOK.md`、release notes、两个 OpenSpec change。
- 发布：`v0.6.1`；`v0.6.0` 保持不可变。修复是 Windows 关键路径，其他平台同样走
  `pg_ctl start`，但不再依赖管道 EOF，行为更稳。
