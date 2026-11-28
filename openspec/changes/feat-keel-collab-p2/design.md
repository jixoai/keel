# Design: feat-keel-collab-p2

## 1. Assignment 绑定唯一性（P2-4）

Assignment 是 WorkItem → Bot/Session 的绑定，生命周期独立于 Session；但一个 session
在同一时刻只能绑定一个未终结的 Assignment。当前缺口是“只检查 index 里的
WorkItemID”，而手工接管、重启恢复或跨入口写入的 session 可能 index 未带 WorkItem，
却已经出现在另一个 WorkItem 的 assignment 行里。

```text
Assign(worker session S, work item W)
  ├─ S 在 index 中必须 active
  ├─ S.WorkItemID 非空且 != W → session_already_assigned
  ├─ 扫描 tenant 内 S 的未终结 assignment：
  │    任一条 WorkItemID != W → session_already_assigned
  │    任一条 WorkItemID == W → assignment_already_bound（同一 session 不重复绑定）
  └─ 通过后 CreateAssignment + SetWorkItem
```

错误分类不再复用 `invalid_creation`：

| 条件 | sentinel | code |
|---|---|---|
| Assignment 已绑定另一个 session | `ErrAssignmentBound` | `assignment_already_bound` |
| Assignment 已 done/failed/cancelled 后复用 | `ErrAssignmentTerminal` | `assignment_terminal` |
| Session 已绑定其他 WorkItem | `ErrSessionAssigned` | `session_already_assigned` |
| 幂等键复用但 Bot/session 参数不一致 | `ErrAssignmentConflict` | `assignment_conflict` |

`WorkStore` 增加 `ListAssignmentsBySession(ctx, tenant, session)`。Memory 实现扫描；
PG 实现按 `(tenant_id, session_id)` 查询。CreateSession 仍只允许绑定未绑定 Assignment；
`finishCreation` 的并发绑定失败改为 `assignment_already_bound`。

## 2. Link 完整性与去重（P2-5）

Link 是语义关系，不是父子假设。实现三条规则：

1. **端点完整**：`session` / `work_item` / `assignment` 端点必须在同租户内存在；
   `bot` / `channel` 只要求非空（无存在性端口）；端点类型仍在闭集内。
2. **确定性 ID**：未提供 ID 时
   `uuid.NewSHA1(ns, "keel.collab.link.v1|tenant|workItem|kind|fromType|fromID|toType|toID")`。
   同一关系重复 AddLink 命中同一 ID，store 返回原记录（幂等）。
3. **自然键去重**：store 以 `(tenant, COALESCE(work_item_id), kind, from, to)` 为自然键；
   显式 ID 与既有 ID 内容不一致返回 `link_conflict`；显式 ID 不同但自然键相同返回既有
   记录（不允许同一关系多条）。
4. **跨 WorkItem 可读**：`ListLinks(tenant, workItemID)` 返回 `work_item_id` 等于该 item
   的记录，以及端点为该 item 的记录。这样 A↔B 的关系从 A、B 两侧都可见，且不复制行。

```text
Link{WorkItemID?: W, From: item:A, To: item:B}
  ├─ From/To 各自 GetWorkItem 同租户校验
  ├─ 自然键 = (tenant, W|nil, delegation, work_item:A, work_item:B)
  └─ AddLink 幂等；Links(A) 与 Links(B) 都能读到
```

## 3. per-request tenant registry（P2-6）

装配期单租户 registry 无法服务“请求带 tenant”的 Host 调用。新增：

```go
type ModeRegistrySource interface {
    Registry(ctx context.Context, tenantID uuid.UUID) (*ModeRegistry, error)
}

type TenantRegistries struct { factory func(ctx, tenant) (*ModeRegistry, error); cache map[uuid.UUID]*ModeRegistry }
```

- 首次请求某租户时调用 factory 构建，校验返回 registry 的 tenant 与请求一致；
  之后同租户复用（mutex + map，`-race` 安全）。
- 未知租户由 factory 拒绝，返回 `ErrUnknownTenant` / `tenant_unknown`。
- `HostOptions` 增加 `Registries ModeRegistrySource`，与静态 `Modes` 二选一：
  - 静态模式保留给既有单租户装配与测试；
  - 提供 `Registries` 时 Host 每个方法先按请求 tenant 解析 registry，再执行原有
    RequireEnabled/Descriptor/Enabled 逻辑；审计回退 tenant 也使用请求 tenant。
- production `newCollabModes` 变为 per-tenant factory，`newCollabHost` 注入
  `TenantRegistries`；superagent 与 capabilities 仍解析部署租户的具体 registry。

## 4. Checkpoint summary hash

- schema：`checkpoints.summary_hash text NOT NULL DEFAULT ''`。
- `kernel.Checkpoint` / `CheckpointRecord` / `store` 同名类型增加 `SummaryHash`。
- 写入：kernel 计算 `sha256(summary)`（调用方显式提供且不一致则拒绝）；store/Mem/PG
  在为空时也补算，保证投影行永远有哈希。
- compaction 事件 payload 增加 `summaryHash`，与投影行独立记录同一摘要的哈希。
- 恢复 `corruptCheckpointReason`：
  - 行内哈希非空且与 `sha256(Summary)` 不一致 → corrupt；
  - 事件 payload 带 `summaryHash` 且与行哈希不一致 → corrupt；
  - 旧行/旧事件哈希为空时跳过该项校验（不是兼容层，而是缺失的完整性事实不伪造）。
- 不一致一律回退事件全量重建，并计 `corrupt` 指标。

## 5. requestId 幂等独立于 prune

新增表：

```sql
checkpoint_requests (
  tenant_id, session_id, request_id,
  upto_seq, kept_from_seq, summary, summary_hash,
  tokens_before, tokens_after, policy_version, created_at,
  PRIMARY KEY (tenant_id, session_id, request_id)
)
```

- 保存 manual checkpoint 时在同一事务内写 `checkpoints` 与 `checkpoint_requests`；
  同 requestId 同 upto_seq 更新，不同 upto_seq 拒绝（沿用现有唯一性）。
- `CheckpointByRequest` 读 `checkpoint_requests`，不依赖 `checkpoints` 是否存在；
  prune/TTL 只操作 `checkpoints`，幂等事实永久保留。
- MemStore 维护独立的 `checkpointRequests` map 模拟同一契约（PG parity）。
- 结果结构仍返回 `Checkpoint`：摘要、tokens、kept seq、policy version 均从幂等表读取，
  因此投影被 prune 后重复 requestId 仍返回同一结果、不再调用模型。

## 6. TTL 惰性清理

TTL 不再只绑定“写新 checkpoint 之后”：

```text
session Run 开始（读取事件、构造 model view 之前）
  → 解析一次生效策略
  → MaxCheckpoints>0 || TTL>0 时调用 PruneCheckpoints
  → 再 LoadCheckpoint / 恢复
```

- 复用既有 `CheckpointPruner` 与 `PruneRequest`，不新增第二套清理语义。
- 最近一个已锚定 checkpoint 仍由 store 保护；事件永不删除；幂等表不删除。
- `CompactNow` 已在保存后 prune；本 change 增加读取/运行路径的惰性触发，使长期不写新
  checkpoint 的会话也会在下次活动时清掉过期旧投影。
- 清理失败 best-effort：不影响 run，记录 warning/`prune` 指标（既有行为）。

## 7. 验证

- `go test -race ./internal/collab/... ./internal/kernel/... ./internal/agent/...`
  （agent/kernel 按仓库方式，PG 用例仅在 `KEEL_TEST_DSN` 设置时运行）。
- `openspec validate --strict feat-keel-collab-p2`。
- 不修改 archived change 的 tasks；本 change 的 tasks 记录实际完成项与 BLOCKED。
