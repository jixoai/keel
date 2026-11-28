# collaboration-core Delta

## ADDED Requirements

### Requirement: Assignment 跨 WorkItem 绑定唯一

一个 Session MUST 在同一时刻只绑定一个未终结的 Assignment；Assign/CreateSession MUST
同时校验 index 的 WorkItem 归属与持久化 assignment 行，MUST NOT 只信任单一投影。
错误 MUST 可分类：已绑定 `assignment_already_bound`、终态复用 `assignment_terminal`、
session 已属其他 WorkItem `session_already_assigned`、幂等参数冲突
`assignment_conflict`；MUST NOT 统一折叠为 `invalid_creation`。

#### Scenario: 已绑定其他 WorkItem 的 session
- **WHEN** worker session 已通过另一 WorkItem 的 assignment 绑定（即使 index 未带
  WorkItemID），调用方再对当前 WorkItem Assign
- **THEN** 返回 403 `session_already_assigned`，不创建第二个 assignment

#### Scenario: 终态 assignment 复用
- **WHEN** assignment 已 done/failed/cancelled，CreateSession 再次引用它
- **THEN** 返回 `assignment_terminal`，不启动新 Session

#### Scenario: 幂等重放参数一致
- **WHEN** 同一 WorkItem 使用相同 idempotencyKey 且 Bot/session 参数相同重试 Assign
- **THEN** 返回原 assignment，不创建第二行、不重复审计

### Requirement: Link 关系完整且可幂等去重

Link MUST 校验端点类型与同租户存在性：session/work_item/assignment 端点 MUST 指向
同租户真实记录；bot/channel 端点只要求非空。无显式 ID 时 MUST 按
(tenant, workItem, kind, from, to) 生成确定性 ID；同一自然关系重复 AddLink MUST
返回原记录，显式 ID 内容冲突 MUST 返回 `link_conflict`。Link MUST 支持跨 WorkItem
关系，并 MUST 能从任一端点 WorkItem 读取，不复制行。

#### Scenario: 跨 WorkItem 关系双端可读
- **WHEN** 记录 work_item A → work_item B 的 review Link
- **THEN** `Links(A)` 与 `Links(B)` 都包含该 Link，且只存在一行

#### Scenario: 重复 AddLink
- **WHEN** 同一关系在没有显式 ID 的情况下重复提交
- **THEN** 返回同一 linkId，store 不新增第二行

#### Scenario: 跨租户端点
- **WHEN** session 端点属于另一个租户
- **THEN** AddLink 返回 `invalid_request`/not-found，不写入 Link

### Requirement: 按请求租户解析 ModeRegistry

Host MUST 能按请求 tenant 解析该租户的 ModeRegistry，而不是只使用装配期单租户对象。
同一租户 MUST 复用同一个 registry；未知租户 MUST 返回类型化拒绝
`tenant_unknown`；factory 返回的 registry MUST 与请求租户一致。单租户静态装配
MUST 继续可用且行为不变。

#### Scenario: 两个租户各自注册表
- **WHEN** Host 先以 tenant A 调用 OpenWorkItem，再以 tenant B 调用
- **THEN** 两次分别使用 A/B 的 registry，A 的模式不出现在 B 的解析结果

#### Scenario: 未知租户
- **WHEN** factory 拒绝租户 C
- **THEN** 调用返回 `tenant_unknown`，不创建 WorkItem/Session，并写拒绝审计

#### Scenario: 并发解析
- **WHEN** 多个 goroutine 同时以同一租户首次调用
- **THEN** factory 最多构建一次，返回同一 registry 指针，`-race` 无竞态
