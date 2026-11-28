# Proposal: keel-board-sync-v2

## Why

board Phase 2 归档时仍有两个 API 部分项：sync targets 列表、最老队列 lag、24h 失败数。board v0.1.1 没有这些字段，keel mount/UI 只能显示队列深度。同时 keel-board 的 domain/server/sync 仍直接依赖 PostgreSQL，存储边界不清晰；后续若做 SQLite 或其它方言会被耦合阻塞。

## What Changes

### 1. Storage dialect boundary（board v0.1.3 → v0.1.4）

- keel-board 的 append log/store/sync/audit/automation 定义 dialect-neutral 接口；`pgx`、SQL、事务、迁移、advisory lock、lease 语义收进 `pgadapter`。
- `pgadapter/logstore` 只承载日志适配器，`pgadapter/schema` 独占 embedded migrations；`pgadapter` 承载 syncstore/audit/automation。
- `storage` 声明 capability（durable / cross-process leases / migrations / listen-notify），未声明能力必须返回 typed `storage.ErrUnsupported`。
- `conformance.RunStorage` 只依赖接口；PostgreSQL 与 in-memory 适配器跑同一套 suite。
- `go list -deps` 门禁动态发现模块包（不手列），证明 store/syncstore/audit/automation/server/sync/log 不依赖 pgx。
- v0.1.4 在 `cmd/board` 与 keel mount 装配处调用 `Capabilities.Require`；未声明能力在启动时以 typed error 拒绝。

### 2. Board sync API v0.1.2

- board v0.1.2（已发布，不可重打）提供只读健康：`targets[]`、`lag.{inbox,outbox}`（outstanding / oldestAt / ageSeconds）、`failures24h`；缺字段为 null，不伪造 0。
- 存储边界用 `v0.1.3`；装配门禁与发现式边界证明用 `v0.1.4`。API 与 v0.1.2 相同；keel 依赖 `v0.1.4`。
- keel mount 映射到 `/v1/board/sync`；权限仍走 `board.manage`。mount 在 board 未提供扁平字段时由 `lag` 派生 `oldestPendingAgeSeconds`，无 outstanding 时为 null。
- UI 用真实字段替换“后端未上报”的占位；无数据时显示真实空态，不伪造。

### 3. Board P2 closure

- watcher dead/lastError、delivery depth.dead 透出；390 选中 state tab 自动 reveal/滚入；筛选 sheet body 可滚、footer 可换行。
- provider 错误默认产品文案；任务外部副本提供“查看原始错误”显式 disclosure，原始句子不进入首屏。
- 移动端复制链接；automation 下次日期/危险权重；文案去 meta。

## Out of scope

- 不实现 SQLite 生产后端；不重开已归档的 `keel-board-v1`；不碰酒店/BU/宿主。
- 不在本 lane 跑全量 TUI/E2E；由最终集成批次执行。
