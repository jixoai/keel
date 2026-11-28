# Tasks: keel-plugin-split

Evidence: `evidence/storage-boundary.md`, `evidence/verification.txt`.

## 0. Plugin storage contract

- [x] plugin kernel：tenant-scoped plugin_state/config/dependency；install inventory 由 core store/harnessext 单一 owner
- [x] domain store interface + PG adapter + conformance；board/superagent 各用自己的 store
- [x] attachment/telemetry/commands/UI/MCP/skills 明确不新增独立 domain DB
- [x] no SQLite；保留 adapter 边界

## 1. Board plugin

- [x] board 插件化；暴露 task/channel/thread/append/query 服务
- [x] board 存储/API 不 import superagent、不 import 旧 bridge
- [x] board 加载/卸载/禁用测试

## 2. Superagent plugin

- [x] superagent 插件化；ticket/assignment/attention/outbox/subscription/cursor/report/lease 自有存储
- [x] `requires: [board]`；board 缺失 → PENDING
- [x] 删除 `internal/superagentboard` bridge/mirror 和双向直接依赖
- [x] 通过 board service 读写任务面；无 board 表访问

## 3. Breaking integration

- [x] 重建 board/superagent 表；不保留旧兼容层/双写
- [x] 插件依赖拓扑装载/反序卸载测试
- [x] E2E：board 插件任务 → superagent ticket/assignment → board 回写
- [x] `go test -p 1 ./...`、nsh check、OpenSpec strict
- [ ] 独立子代理复审 P0/P1=0；archive

## 4. Independent review fixes (2026-11-28)

- [x] P1-1 导入循环自冲突：`ImportBoardTask` 先按 `board_task_id` 查 link 并跳过投影/非可执行状态；`PutBoardLink` 对 board_task_id 冲突幂等返回已有 link；两轮 `Writer.SyncOnce + Import` PG 测试
- [x] P1-2 导入 assignment 调度：importer 不再写无 session 的 open assignment；传 `WorkerBot` + orchestrator owner session，经 `Orchestrator.DispatchTicket` 建带 worker session 的 assignment；测试断言 session/Bot
- [x] P1-3 serve 半加载：supervisor/writer 在 `superagentplugin.Options.Build` 内、board 发布后构建；board 缺失/失败 → PENDING；product 暴露按 kernel Active 门禁；装配级测试
- [x] P2-1 清理已删 `cmd/board-sync-e2e` / `internal/superagentboard` 的脚本、coverage 规则与 RUNBOOK 死引用
- [x] P2-2 `ready.go` 覆盖 `plugin_state/config/dependency` 与 `superagent_*`，含回归测试
- [x] P2-3 架构测试扫描 core store SQL 表名；断言 importer 使用 `GetBoardLinkByTask`
- [x] P2-4 前端删除 `mirror.readOnly/bridge` 与命令桥 `answer` 动作，补 board 数据测试
- [x] P2-8 pluginruntime reload / requires-by-id 测试；superagent Stop 可重入，二次 Stop 不泄漏
- [x] P2-9 只导入可执行 board 状态；link 存在但无 assignment 时重复 dispatch 可修复
- [x] P2-6 旧 collab_*/board_links 非空时拒绝迁移并给出导出提示；空表仍删除
- [x] P2-5 证据修正：board pin v0.1.4；verification.txt 绑定 `01912296`；E2E 跑两轮生产 `Writer.SyncOnce` 并断言 worker session/Bot
- [x] P2-7 `plugins` inventory 单一 owner（core store / harnessext）；删除 pluginstore 未用 install 写入面
