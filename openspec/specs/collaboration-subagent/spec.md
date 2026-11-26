# collaboration-subagent Specification

## Purpose
TBD - created by archiving change keel-collab-subagent. Update Purpose after archive.

## Requirements

### Requirement: sub-agent 是树形委派模式

系统 SHALL 提供 `subagent` 协作模式：一个 main Bot 角色在本次 WorkItem 中派生一个或多个
sub Bot 角色，形成树形委派；main/sub 是本次工作的位置，MUST NOT 成为 Bot kind 或永久
组织归属。同一个 Bot MUST 能在不同 WorkItem 中作为 main 或 sub。

#### Scenario: 同一角色切换位置
- **WHEN** B-bot-1 在一个 WorkItem 中是 A-bot-1 的 sub，在另一个 WorkItem 中是 main
- **THEN** 两次运行使用同一个 Bot/权限边界，只改变本次 mode 的位置

### Requirement: 深度语义与上限

系统 MUST 定义 main depth=0，每次 spawn 子 depth=父 depth+1；`maxDepth=3` MUST 允许
depth 0..3；`maxDepth=-1` MUST 表示不限；`maxDepth=0` MUST 禁止派生。创建子 Session 前
MUST 检查 `depth+1 <= maxDepth`，超限返回类型化错误 `subagent_depth_exceeded`，且不创建
子 Session、不写孤儿。

#### Scenario: 默认三层
- **WHEN** maxDepth=3 且 main 连续派生三层
- **THEN** depth 3 的 sub 仍可运行；depth 3 再派生 depth 4 被拒绝

#### Scenario: 不限深度
- **WHEN** maxDepth=-1
- **THEN** 不做深度上限拒绝，但仍执行预算/并发/权限检查

#### Scenario: 禁止派生
- **WHEN** maxDepth=0
- **THEN** main 的任何 spawn 都返回 `subagent_depth_exceeded`

### Requirement: 单一 spawn 门与 durable 链接

`agent` 工具、`/v1/sessions/{id}/subagents`、触发器/插件入口 MUST 共用同一 Host spawn
检查。子 Session 的第一条 durable 记录 MUST 包含 `mode=subagent`、`parentSessionId` 和
`depth`；重启后 MUST 从账本恢复父子与深度，不依赖进程内 registry。

#### Scenario: 重启恢复深度
- **WHEN** host 在 depth 2 的子任务运行中重启
- **THEN** 重启后仍能从 durable link 得到 depth 2，并继续执行 depth 限制

#### Scenario: 旧入口一致
- **WHEN** 通过 `agent` 工具或 HTTP 路由 spawn
- **THEN** 两条路径的权限/深度/预算拒绝结果一致

### Requirement: 预算、扇出与取消

Host MUST 对每个 sub-agent 执行 turns/tokens/timeout 预算和 `maxChildren` 直接子节点上限。
父 WorkItem 取消时 MUST 按策略取消未终态子树；MUST NOT 产生孤儿 Session。每次 spawn/拒绝
MUST 写审计，且审计不含任务正文/密钥。

#### Scenario: 超扇出拒绝
- **WHEN** 父节点已有 maxChildren 个直接子节点，再请求一个
- **THEN** 返回 `subagent_children_exceeded`，不创建新 Session

#### Scenario: 取消传播
- **WHEN** 父 WorkItem 被取消
- **THEN** 未终态子节点被取消/标记为取消，审计记录父/子 id 与原因

### Requirement: 权限与租户边界

spawn MUST 按目标 Bot 的 `bot.invoke` 授权；子 Session MUST 使用目标 Bot 自己的
`Account ∩ Harness` 权限，MUST NOT 自动继承父 Bot 权限或做调用链交集。子 Session MUST
与父 Session 同租户；跨租户 spawn MUST 拒绝。

#### Scenario: 缺 bot.invoke
- **WHEN** 父 Bot 对目标 sub Bot 没有 `bot.invoke`
- **THEN** spawn 返回 403，写 `permission.denied`，不创建子 Session

### Requirement: fork 不计入委派深度

fork MUST 继续按 eventSeq 创建账本分叉；fork 是历史分叉，MUST NOT 消耗 sub-agent 的
depth 或 `maxChildren`。只有 spawn 委派树计入深度。

#### Scenario: fork 不受深度限制
- **WHEN** depth 3 的 sub 执行 fork
- **THEN** fork 按既有 eventSeq 规则成功，不因 sub-agent depth 被拒绝

### Requirement: 结果与 UI

父 Session MUST 收到结构化结果（status/summary/artifacts/usage）；web/TUI MUST 可查看
子代理树、深度、状态与取消/重试动作。UI MUST NOT 提供“新建 Bot kind=sub”的入口。

#### Scenario: 树可见
- **WHEN** main 派生两个 sub，其中一个再派生一个 sub
- **THEN** 会话页/TUI 展示 depth 0/1/2 的树和各自状态
