# Proposal: keel-db-auto

> 立项：2026-11-24（Owner 追加要求：不应该让用户手动输入 `keel db`，默认应自动初始化；特殊需求才显式管理）。

## Why

`keel-db-cli` 把数据库管理收进了 CLI，但 quickstart 仍要求用户先跑 `keel db start` 再
`eval "$(keel db env)"`。这不是产品该有的默认体验：本地开发库是实现细节，用户只想启动产品。
显式的 `keel db` 应保留给特殊需求（换端口/目录、查看状态、停止、重置），而不是必经步骤。

## What Changes

1. **`keel serve` 自动初始化**：`KEEL_DSN`/`--dsn` 未设置时，自动确保本机开发集群可用
   （缺则 `initdb`、停则启动、缺库则建库）并继续启动；stderr 只提示数据目录，不打印密码。
2. **`keel admin` 同样自动**：`bootstrap`/`reset`/租户/配额等命令在无 DSN 时复用同一本地集群，
   这样官网 quickstart 的 bootstrap 不需要先手动建库。
3. **特殊需求可关闭**：`--no-auto-db` 或 `KEEL_NO_AUTO_DB=1` 时，`serve`/`admin` 在无 DSN 下
   必须以清晰错误退出（提示 `keel db start` 或设置 `KEEL_DSN`）；一旦设置 `KEEL_DSN`，
   MUST 只使用显式 DSN，绝不自动初始化。
4. **文档**：官网首页/核心页 quickstart 删除 `keel db start`/`eval` 步骤，改为
   build → bootstrap → serve（首次自动建库）；README 快速开始改用 `keel serve`；
   RUNBOOK 说明自动行为与关闭方式；测试页仍显式 `keel db`（测试需要 `KEEL_TEST_DSN`，属特殊需求）。
5. **验证**：无 DSN 下 `admin bootstrap` + `serve` health 200；`--no-auto-db`/env 两种关闭路径
   exit 2；`go test ./cmd/keel`；站点四道门 + 部署；报告更新。

## Impact

- 代码：`cmd/keel/db.go`（抽取 `ensure`/`ensureAutoDevDB`）、`cmd/keel/main.go`（serve）、
  `cmd/keel/admin.go`（adminStore）。
- 文档：README、RUNBOOK、官网首页/核心页 EN+ZH。`keel db` 子命令本身不变。
- 安全边界：自动初始化只在无 DSN 时发生；生产设置 `KEEL_DSN` 即可完全绕过；
  本地库仍只绑回环、随机 0600 密码。
