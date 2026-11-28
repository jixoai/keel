# Proposal: keel-board-sync-v2

## Why

board Phase 2 归档时仍有两个 API 部分项：sync targets 列表、最老队列 lag、24h 失败数。board v0.1.1 没有这些字段，keel mount/UI 只能显示队列深度。同时 keel-board 的 domain/server/sync 仍直接依赖 PostgreSQL，存储边界不清晰；后续若做 SQLite 或其它方言会被耦合阻塞。

## What Changes

### 1. Storage dialect boundary

- keel-board 的 append log/store/sync/audit 定义 dialect-neutral 接口；PG 适配器移到独立包。
- 事务、advisory lock、LISTEN/NOTIFY、迁移、JSON、lease 语义由 dialect 显式声明，不假设所有后端都支持。
- conformance 测试只依赖接口；PG 是生产实现。SQLite 本轮不实现，只保留 adapter 边界。

### 2. Board sync API v0.1.2

- keel-board 增加只读 sync status：targets、oldest pending age、24h failures（或等价字段）。
- keel mount 映射到 `/v1/board/sync`；权限仍走 board.read/manage。
- UI 用真实字段替换“后端未上报”的占位；无数据时显示真实空态，不伪造。

### 3. Board P2 closure

- watcher dead/lastError、delivery depth.dead 透出；390 选中 tab 自动滚入/短屏筛选；provider 错误详情保留在服务端但可展开；移动端复制链接；automation 下次日期/危险权重；文案去 meta。

## Out of scope

- 不实现 SQLite 生产后端；不重开已归档的 `keel-board-v1`；不碰酒店/BU/宿主。
