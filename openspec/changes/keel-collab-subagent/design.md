# Design: keel-collab-subagent

## 1. 拓扑

```text
WorkItem: 任务
  main: A-bot-1 (depth 0)
    ├─ sub: B-bot-1 (depth 1)
    ├─ sub: B-bot-2 (depth 1)
    │    └─ sub: C-bot-1 (depth 2)
    └─ sub: C-bot-2 (depth 1)
```

- 同一 Bot 可在不同 WorkItem 中处于不同位置。
- `maxDepth=3` 允许 depth 0..3；`-1` 不限；`0` 禁止派生。
- `maxChildren` 限制每个父节点的直接子数，防止单层扇出过大。
- fork 不在这棵树里计数；fork 是账本分叉。

## 2. 数据与恢复

子 Session 的第一条 durable 输入/事件写入：

```json
{"subagent":{"mode":"subagent","parentSessionId":"…","depth":1,"maxChildren":8}}
```

- 不新增 `bots.kind`，不把 depth 写成 Bot 属性。
- `DurableParentSession` 扩为 `DurableSubagentLink`，读取 parent + depth + mode；
- 重启后从账本恢复，不依赖 `SubagentRunner` 的进程内 registry；
- 旧记录缺 depth 时 fail closed（Alpha 零兼容），或从父链重算后拒绝超限。

## 3. Host 流程

```text
spawn request
  → 解析当前 Session 的 subagent link（depth）
  → 解析目标 Bot / 权限 / budget
  → depth+1 > maxDepth? 拒绝
  → children >= maxChildren? 拒绝
  → 检查 cycle/self-spawn 策略
  → Host.CreateSession + durable link
  → 审计 subagent.spawned / subagent.refused
```

所有入口（`agent` 工具、HTTP、触发器/插件）只构造 `SpawnRequest`；Host 是唯一检查点。

## 4. 取消与预算

- 父取消 → 子树按 Host 策略取消（默认取消所有未终态子节点）。
- 父结束但子仍运行：按现有产品规则，子可继续；结果写入父 WorkItem 的只读结果。
- 深度/子节点数/ turns/tokens/timeout 超限在创建前拒绝，不产生孤儿 Session。
- 审计只记录 ids/原因/预算数字，不记录任务正文。

## 5. UI

- 会话页展示子代理树：角色、状态、深度、tokens/turns、取消/重试。
- TUI 用文本树 + 选中行动作；headless/phone 用折叠列表。
- 不新增“新建 Sub Agent”实体入口；入口是“以某个 Bot 角色派生子任务”。

## 6. 兼容

- `agent` 工具现有 action 保持；新增可选 `maxDepth`（收窄）和结果中的 `depth`。
- HTTP `/v1/sessions/{id}/subagents` 保持；新增 depth 拒绝码。
- 旧无 depth 数据按 fail closed；不写兼容垫片。
