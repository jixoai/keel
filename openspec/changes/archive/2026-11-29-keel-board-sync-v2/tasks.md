# Tasks: keel-board-sync-v2

> 证据：`evidence/storage-boundary.md`、`evidence/ui-gates-20261128T131744Z/`（55 张实拍）、
> `evidence/verification.txt`。board `v0.1.2`（`006a43b`）已发布不可重打；存储边界交付
> `v0.1.3`（`80bff1b`）与装配门禁 `v0.1.4`（`3d6115b`）。

## 0. Storage dialect boundary（board v0.1.3 → v0.1.4）

- [x] 定义 keel-board append log/store/sync/audit/automation 接口与 dialect-neutral 类型
- [x] 将 pgx/SQL/事务/迁移/公告锁/lease 收进 `pgadapter` / `pgadapter/logstore` / `pgadapter/schema`
- [x] 加 `storage.Capabilities` 与 typed `ErrUnsupported`；PG 声明 durable/cross-process/migrations，
      `ListenNotify=false`（Wait 轮询，若加 LISTEN/NOTIFY 只进 `pgadapter/logstore`）
- [x] `cmd/board` 与 keel mount 装配处调用 `Require`；conformance 用双 handle 证明跨进程 lease
- [x] 加 `conformance.RunStorage`；PostgreSQL 与 in-memory 适配器同一 suite 全绿
- [x] `go list -deps` 动态发现模块包，证明 domain/server/sync/log 不依赖 pgx；`TestStorageBoundary` 固化
- [x] 打 board annotated tag `v0.1.3`（边界）与 `v0.1.4`（装配门禁/发现式边界）并推 origin（`v0.1.2` 未动）

## 1. Board sync API v0.1.2

- [x] keel-board targets / lag（outstanding/oldestAt/ageSeconds）/ failures24h 只读字段
- [x] 协议/服务端/store 测试；缺字段保持 null，不伪造 0（board `go test ./...`）
- [x] `v0.1.2` tag 已推 origin（`006a43b`，本批未重打）
- [x] keel `go.mod` 升级到 `v0.1.4`；mount DTO/权限映射（含由 lag 派生的 `oldestPendingAgeSeconds`）
- [x] keel mount contract tests + PG 集成（`KEEL_TEST_DSN=... go test -p 1 ./internal/boardintegration ./cmd/keel`）

## 2. UI

- [x] sync 页显示真实 targets/lag/24h；缺失只留一句弱提示（TUI 同步用例 + `sync.tsx`）
- [x] 390 选中 state tab 自动 reveal/滚入；筛选 sheet body 可滚、footer 可换行
      （`15-queue-archive-390.png`、`49b-filter-sheet-390x667.png`）
- [x] task 页 provider 错误产品文案 + “查看原始错误”显式 disclosure；移动端复制链接可达
      （`50-task-copy-error-disclosure-390.png`）
- [x] board P2：watcher dead/lastError/depth.dead、automation 下次日期/危险权重、去 meta 文案
- [x] 缺失字段 harness（`sync-missing`）：`backend-gap` 可见、lag/failures 指标隐藏；失效 testID 已替换
- [x] fixture/UI 去假字段：删除 board 不存在的 entry `lastDeliveryAt` 与 watcher `successes24h/failures24h`；
      entry 最后投递改由真实 `targets[].lastDeliveryAt` 派生
- [x] `depth.dead` 解析并展示在投递 sheet；watcher 行只显示真实 lastDeliveryAt
- [x] board TUI/UI 证据更新：web capture 6/6 通过、55 张 390/1440/768 实拍；TUI 定向 13/13

## 3. Gates and archive

- [x] board `go test ./...` + `go test -race ./...` + PG conformance（`KEEL_BOARD_TEST_DSN`）
- [x] `nsh check`；board data 24/24、SSE 3/3、TUI 定向 13/13；全量 TUI 留给最终集成批次
- [x] keel `go test -p 1 ./internal/boardintegration ./cmd/keel`（DSN 集成）
- [x] 独立子代理复审 P0/P1=0（PASS；P2 closed in evidence/p2-closeout.md；最终门禁 GATE PASS）
- [x] 更新 README/RUNBOOK/DISPOSITION；`openspec archive keel-board-sync-v2 --yes`
- [x] 发 v0.6.0（https://github.com/jixoai/keel/releases/tag/v0.6.0，release workflow success）

## 残余 / 已完成

- 独立安全/功能/视觉 A/B/C 复审 PASS（P0/P1=0）；最终门禁 GATE PASS。
- 全量 TUI（540）、56 张 UI 实拍与 HTML 报告已生成并发布。
- 无 SQLite 生产实现；本轮只保留 adapter 边界与 capability 声明。
