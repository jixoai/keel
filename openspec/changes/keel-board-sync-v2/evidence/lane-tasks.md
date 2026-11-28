# Tasks: feat-keel-board-sync-v2

> 基线：keel `origin/main` `2c1245e9`；board `origin/main` `70b503f`（v0.1.1）。
> 每批一提交、可独立回滚。board v0.1.2 先产出 tag，keel 再升级依赖。
> 证据目录：`openspec/changes/feat-keel-board-sync-v2/evidence/`。

## 0. 规格与基线

- [x] 0.1 proposal / specs 增量与验收口径对齐（每个 Requirement 至少一个 Scenario）
- [x] 0.2 受影响文件 / 子仓 / 部署面盘点：board `syncstore`+`server`+`automation`；
  keel `internal/boardintegration`、`web/src/data`、`web/src/screens/board`、kit
  `tabs`/`sheet`；无 schema 迁移、无新权限键、无新部署面

## 1. board v0.1.2：只读健康数据全链

- [ ] 1.1 `syncstore` 增加 `Targets`（按 key 枚举 external target + outbox 聚合）、
  `OldestPending`（最老 pending/leased inbox/outbox 时间戳）、`Failures24h`；memory 与
  PostgreSQL 实现同语义
- [ ] 1.2 `server` DTO：`GET /sync/entries/{key}/targets`、entry status 的
  `targets/lag/failures24h`、`/sync/status` keys 与 root 聚合；错误码仍走
  `board.manage` + 404/403
- [ ] 1.3 `syncstore`/`server` 测试：无 outstanding 时 `lag.inbox/outbox = null`、
  有 pending 时有真实 oldest/age、24h 窗口边界、target 按 key 隔离、跨租户 404、
  不泄漏正文/凭据
- [ ] 1.4 PostgreSQL 测试：`withStores` 覆盖 memory + PG；24h 边界用可控时间/行更新
- [ ] 1.5 `docs/SYNC.md` 记录新只读字段与 null 语义；README 状态提到 v0.1.2
- [ ] 1.6 board commit、annotated tag `v0.1.2`、push 分支 + tag

## 2. board v0.1.2：watcher 运行事实

- [ ] 2.1 `automation` 增加 `WatcherStats` 读取（dead、lastError、lastDeliveryAt），
  memory 与 PostgreSQL 同语义（可选接口，不破坏既有 Store 实现者）
- [ ] 2.2 `server` watcher DTO 透出 `dead/lastError/lastDeliveryAt`；测试覆盖有死信、
  无死信、跨租户、重试后归零

## 3. keel：依赖与 mount

- [ ] 3.1 `go.mod`/`go.sum` 升到 `keel-board v0.1.2`；`go test ./internal/boardintegration`
- [ ] 3.2 augment 不再覆盖 board 的 `targets/lag/failures24h`；仅在字段缺失时保留
  `null`；credentials 合并保持不变；SSE 帧不改写
- [ ] 3.3 真实 PG 集成测试：`/sync/status` 与 `/sync/entries/{key}/targets` 的
  targets/lag/24h 与 board store 写入一致；`lag:null` 只表示无 outstanding 或缺失，
  不伪造 0
- [ ] 3.4 权限/脱敏回归：无 `board.manage` 403；跨租户不串数据；target 不含正文/凭据

## 4. keel：同步页与自动化页真实数据

- [ ] 4.1 `web/src/data/board-wire.ts` / `http.ts` 解析 targets/lag/failures24h、
  watcher dead/lastError/lastDeliveryAt；缺失与 0 区分
- [ ] 4.2 同步页显示 lag（秒/分钟）、24h 失败、目标列表与 undelivered；无积压/未知
  各有文案；保留凭据/死信/失败重试
- [ ] 4.3 自动化页用真实 dead/lastError/lastDeliveryAt 驱动 badge 与重试死信
- [ ] 4.4 fixture 改为 v0.1.2 形状；新增“字段缺失仍诚实”的 harness 场景与断言

## 5. keel：P2 UI

- [ ] 5.1 390 状态 tabs 选中项自动滚入视口（`reveal`），保留横向滚动与右缘渐隐
- [ ] 5.2 短屏筛选 Sheet footer 可换行，390×667 无横向溢出、按钮可达
- [ ] 5.3 provider 错误按产品口径：类别 + 下一步；原始 provider 句子不出现在 UI
- [ ] 5.4 移动端任务复制链接恢复（44px）
- [ ] 5.5 schedule 下次运行显示日期；取消使用危险权重 + AlertDialog
- [ ] 5.6 删除 takeover/forceRelease 客户端死代码（API/fixture/move/label），保留 answer

## 6. 验收与证据

- [ ] 6.1 board：`go test ./...`、`go test -race ./...`、`KEEL_BOARD_TEST_DSN=... go test ./...`；
  记录命令/退出码/关键行
- [ ] 6.2 keel：`go test -p 1 ./internal/boardintegration ./cmd/keel`；`cd web && nsh check`；
  定向 TUI（board data/SSE/同步/自动化/390）；不跑全量 TUI
- [ ] 6.3 变更文件 before/after 说明与 UNCOVERED 清单（定向范围）；不伪造截图
- [ ] 6.4 回写 specs / DISPOSITION 残余（P2 #8/#9/#10 关闭，takeover/forceRelease 删除）

## 审查门禁（交付前必须全勾；缺一不得声称「完成」）

- [ ] **A 安全红队**（独立子代理）：越权 / 租户隔离 / SSRF / 注入 / 令牌；攻击脚本全被拒。
  - 证据：`evidence/security-review.md`
  - 状态：BLOCKED — 本 lane 为单实现子代理，无法自行完成独立子代理复核；由主 Agent 最终集成批次执行
- [ ] **B 功能与链路红队**（独立子代理）：跨模块链路逐条「上游产出 → 下游可见可处理」可复现；
  每个按钮/表单项点击后**数据真实变化 + UI 反映 + 失败可读**；五态（加载/空/错/权限不足/成功）齐备；
  长列表可检索。
  - 证据：`evidence/functional-review.md`
  - 状态：BLOCKED — 同上
- [ ] **C 视觉与设备红队**（独立子代理）：375×667 / 390×844 / 768×1024 / 1440×900 四档截图；
  「外层文档不滚、内部容器滚」（或按页面语义反向）程序化断言；玻璃材质、对比度、before/after 审计。
  - 证据：`evidence/visual-review.md`
  - 状态：BLOCKED — 同上；本 lane 只跑 390 TUI 几何断言
- [ ] **P0/P1 清零**；P2 已进 backlog 并标注去向
- [ ] 缺陷分级表已回填（P0/P1/P2 数量、修复提交、豁免理由）

## 验收

- [ ] 相关单测 / 脚本 dry-run 输出已贴报告（命令 + 退出码 + 关键行）
- [ ] E2E HTML 图文报告（脚本生成，禁手写；每步截图）
  - 状态：BLOCKED — 由最终集成批次执行；本 lane 不运行全量 E2E
- [ ] 交付链接非 localhost / 127.0.0.1（三形态之一）
  - 状态：不适用 — 本 change 无部署链接
- [ ] 规格变更已回写 change（不得只改代码不改 specs）
