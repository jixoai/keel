# session-binding Specification

## ADDED Requirements

### Requirement: 会话当前绑定是账本的派生索引

会话的当前执行绑定 MUST 由「最新一条携带 selection 的 `user.message`」派生，并记录在
`sessions.bound_agent_id` / `bound_team_id` / `bound_seq` / `bound_at`。事件账本仍是唯一真值：
这些列 MUST NOT 参与 run 装配（attempt 仍按账本 per-attempt 解析 selection），也 MUST NOT
成为第二份历史；历史绑定查询 MUST 以事件为准。

- 写入 MUST 发生在事件提交成功之后，并以该事件的 seq 作条件（`bound_seq <= incoming seq`）：
  同一会话的并发输入即使提交后的写落序颠倒，MUST 由更大的 seq 胜出。
- `selection` 同时命名 `teamId` 与 `agentId` 时 MUST 记为 team 绑定（team 决定 captain），
  `bound_agent_id` 为空。
- 只命名模型的 selection MUST 清空两个 id（最新输入是权威的「不绑定」决定）；
  不带 selection（或显式 null 被入口拒绝）的输入 MUST NOT 改变已有绑定。
- 去重（deduped）的请求 MUST 按已提交事件的 payload 投影，MUST NOT 按请求体重写绑定。
- 归档会话 MAY 保留绑定列作诊断，但归档 MUST NOT 让绑定列成为新的历史真值。

#### Scenario: 团队输入绑定
- **WHEN** 会话收到 `selection={providerId, modelId, teamId}`
- **THEN** `bound_team_id` 为该团队、`bound_agent_id` 为空，`bound_seq` 为该 `user.message` 的 seq

#### Scenario: 模型-only 输入解绑
- **WHEN** 会话此前绑定某 Agent，随后收到只含模型键的 selection
- **THEN** 两个绑定 id 被清空，历史 `user.message` 仍保留原 selection

#### Scenario: 并发输入
- **WHEN** 同一会话并发提交多个带不同 `agentId` 的输入
- **THEN** 最终绑定等于账本中 seq 最大的 selection，而不是最后完成的 HTTP 写

### Requirement: 删除与归档 MUST 保护会话引用

Agent 或 Team 被「活跃会话」的当前绑定引用时 MUST NOT 被硬删除。删除 MUST 返回 409，
code 分别为 `agent_in_use` / `team_in_use`，并在 `references` 中列出引用会话
（`kind:"session"`，含 id、state、runStatus、archived）。Agent 同时被团队引用时 MUST
把团队引用与会话引用放在同一个 `references` 数组中。

- 未归档会话 MUST 视为引用，无论其 run 是 idle、pending、running 还是 paused。
- 已归档会话 MUST NOT 视为引用；但若它仍持有 `pending`/`running` run，MUST 继续视为引用
  （归档不取消 run，删除其 Agent/Team 会破坏下一次 attempt）。
- 引用检查 MUST 按租户隔离；其他租户的同 id 绑定 MUST NOT 阻塞本租户删除，也 MUST NOT
  被列出。
- 归档操作 MUST 复用同一引用检查：被活跃会话绑定时返回同一 409 code 与 references。
  归档一个被团队席位的 Agent MUST 允许（归档是该引用的修复动作），MUST NOT 因团队席位拒绝。
- 解绑路径 MUST 明确：归档该会话，或发送只选模型的新输入清空绑定。删除/归档 MUST NOT
  改写历史 selection，也 MUST NOT 级联删除会话或事件。
- 无引用的 Agent/Team 删除 MUST 不被无关绑定阻塞。

#### Scenario: 删除被会话绑定的 Team
- **WHEN** 活跃会话的最新 selection 为 `teamId=team_a`，用户删除或归档 `team_a`
- **THEN** 返回 409 `team_in_use` 并列出该会话；归档会话后重试成功

#### Scenario: 删除被会话绑定的 Agent
- **WHEN** 活跃会话的最新 selection 为 `agentId=agent_a`，且 `agent_a` 还是某团队成员
- **THEN** 返回 409 `agent_in_use`，references 同时列出团队与该会话

#### Scenario: 已归档会话仍有运行中 run
- **WHEN** 会话已归档但 `runs.status` 仍为 `pending` 或 `running`
- **THEN** 它仍阻塞其绑定的 Agent/Team 删除；run 结束后不再阻塞

#### Scenario: 跨租户
- **WHEN** 租户 B 的会话绑定与租户 A 要删除的 Team 同 id
- **THEN** 租户 A 的删除成功；租户 A 的 409 引用列表不含租户 B 的会话

### Requirement: 归档是独立生命周期操作

`POST /v1/agents/{id}/archive` 与 `POST /v1/teams/{id}/archive` MUST 把对象标记为归档：
Agent MUST 保留可读配置；Team MUST 经 Seal 前移 revision 并更新 hash，使 run manifest 能
区分归档前后版本。归档 MUST NOT 删除行、MUST NOT 改写团队席位、MUST NOT 影响已启动的 run。
归档对象 MUST NOT 再启动新 run；重复归档 MUST 幂等。

#### Scenario: 归档 Team
- **WHEN** 无活跃会话引用时归档一个团队
- **THEN** 团队 `archived=true`、revision 前移；再次归档返回同一状态

#### Scenario: 归档后的历史
- **WHEN** 团队被归档
- **THEN** 历史会话与 run 快照仍可读，归档不删除任何账本事实
