# Proposal: feat-keel-collab-p2

> 立项：2026-11-28。承接已归档 `keel-collab-core` 与 `keel-checkpoint` 的 P2 项。
> 目标：把两条协作线的剩余 P2 收紧为可验证契约，不重开旧 change、不改 archived tasks。

## Why

`keel-collab-core` 与 `keel-checkpoint` 的 P0/P1 已交付，归档时登记了仍未闭合的 P2：

- `keel-collab-core`：Assignment 跨 WorkItem 重绑仍有未覆盖路径、错误分类混在
  `invalid_creation`；Link 只校验端点类型，ID 生成不可幂等、跨 WorkItem 关系不可读；
  ModeRegistry 仍是装配期单租户对象，Host 无法按请求租户解析模式目录。
- `keel-checkpoint`：checkpoint 只存摘要正文，没有摘要哈希，恢复只能做结构校验；
  manual requestId 的去重依赖 checkpoint 投影行，投影被 prune 后同一 requestId 可重复
  压缩；TTL 只在写新 checkpoint 后触发，长期不再压缩的会话不会惰性清理过期投影。

这些都不是新拓扑，而是既有契约的边界收紧。单独承接可避免改动 archived change 的
完成记录，也让定向测试有明确归属。

## What Changes

1. **P2-4 Assignment 跨 WorkItem 重绑**：新增按 session 的全局 assignment 查询；
   Assign 拒绝把已绑定到其他 WorkItem 的 session 再次绑定；CreateSession/Assign 对
   已绑定、终态、幂等参数冲突分别返回稳定错误码（`assignment_already_bound`、
   `assignment_terminal`、`session_already_assigned`），不再统一落到
   `invalid_creation`。
2. **P2-5 Link 完整性**：端点除类型/ID 格式外，校验 session/work_item/assignment
   在同租户内存在；无显式 ID 时按 (tenant, workItem, kind, from, to) 生成确定性 ID；
   同一自然关系重复 AddLink 返回原记录；显式 ID 冲突返回 `link_conflict`；跨 WorkItem
   关系（端点为两个 WorkItem，或 scope 为空）可按任一端点 WorkItem 读取。
3. **P2-6 per-request tenant registry**：新增按租户解析/缓存 ModeRegistry 的
   `TenantRegistries`；Host 在每次请求按 tenant 解析 registry（仍支持单租户静态装配），
   未知租户返回类型化错误；production 装配改为提供 registry 工厂而不是单一对象。
4. **Checkpoint summary hash**：checkpoint 记录 `summary_hash`（sha256）；compaction
   事件 payload 同步写 `summaryHash`；恢复校验摘要与哈希、投影与事件是否一致，不一致
   fail closed 回退事件全量重建。
5. **requestId 去重独立于 prune**：新增 `checkpoint_requests` 幂等表，manual requestId
   的结果独立于 `checkpoints` 投影保留；prune 只删投影，不删幂等事实，重复 requestId
   在投影被清理后仍返回原结果且不再次调用模型。
6. **TTL 惰性清理**：会话在读取/恢复路径上按当前生效策略惰性执行 TTL/Max 清理，不再
   只依赖“下一次写 checkpoint”；最近一个有效 checkpoint 仍被保护，事件永不删除。

## Capabilities

### New Capabilities

<!-- 无新增 capability；本 change 只收紧既有契约。 -->

### Modified Capabilities

- `collaboration-core`：Assignment 绑定唯一性/错误分类、Link 完整性/去重/跨 WorkItem
  读取、按请求租户解析 mode registry。
- `checkpoint-policy`：summary hash、manual requestId 幂等独立于 prune、TTL 惰性清理。

## Impact

- `internal/collab`：errors、store 端口/MemoryWorkStore、host 绑定与 Link 路径、
  registry 解析；`internal/store/superagent.go` 的 PG 实现补齐端口方法。
- `internal/kernel`：Checkpoint/CheckpointRecord、MemStore/PGStore 的
  `checkpoint_requests` 与 summary hash；`internal/store` 同名实现保持一致。
- `internal/agent`：compaction 事件写 summaryHash；恢复校验；读取路径惰性 prune。
- `cmd/keel`：mode registry 工厂化装配。
- 测试：collab 定向单测（含跨租户/跨 WorkItem/并发）、kernel checkpoint（mem +
  `KEEL_TEST_DSN` 时 PG）、agent 手动压缩/恢复；不改 board/PI/酒店。

## Out of scope

- archived P2 中未列入本 lane 的两项：手动压缩走 hygiene/hooks、手动 claim 不消耗
  attempts（保留在旧 tasks 记录，不在本 change 伪造完成）。
- 多租户 HTTP 路由/认证模型、capabilities 端点的租户化投影（本 change 只保证 Host
  按请求租户解析 registry；进程级只读目录仍按部署租户展示）。
- 新的 Link 关系语义、跨租户链接、软删除/审计历史读取。
