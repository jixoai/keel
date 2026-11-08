# agent-api

> 产品层 AgentAPI（`keel serve`）。内核语义（delivery/inputSeqs/run.finished）在
> `keel-kernel-loop`；本 spec 只描述产品面端点与能力探测。

## ADDED Requirements

### Requirement: 会话列表

`GET /v1/sessions?limit=<n>&after=<cursor>` SHALL 返回当前租户的会话列表（按最后活动倒序，
keyset 分页），每项 MUST 含 `sessionId`、`createdAt`、`updatedAt`、`lastSeq`、`runStatus`、
`lastMessageAt`。跨租户 MUST 不可见。

#### Scenario: 分页与隔离
- **WHEN** 租户 A 有 120 个会话，`limit=50` 连续翻页
- **THEN** 三页共 120 个、无重复无遗漏；租户 B 看不到 A 的会话

### Requirement: 停止当前 run

`POST /v1/sessions/{id}/cancel` SHALL 幂等停止当前活跃 run：在途 attempt 以 context cancel
收口（已提交事件不回滚），run 状态 `cancelled`；重复调用返回同一结果；会话保持可用。

#### Scenario: 取消与完成竞争
- **WHEN** 取消与模型完成同时发生
- **THEN** 只有一个终态；不出现半条 assistant 消息；未答输入仍为未答

### Requirement: 审批答案

`POST /v1/sessions/{id}/decisions/{decisionId}` SHALL 提交答案（`optionId` 或自由文本）：
幂等、租户隔离、答案事件化；重复提交不再产生新事件；超时/取消为终态。

#### Scenario: 重复提交
- **WHEN** 同一 decisionId 提交两次相同答案
- **THEN** 第二次返回 answered 且不产生新事件

### Requirement: token 级 partial（非持久旁路）

`GET /v1/sessions/{id}/events/stream?partials=1` MAY 额外推送 token 级 partial 帧；
partial MUST NOT 写入事件表、MUST NOT 改变 `Last-Event-ID` 语义；未开启时行为与现状一致。

#### Scenario: 断线只补已提交
- **WHEN** partial 期间断线，重连带 `Last-Event-ID`
- **THEN** 只收到已提交事件，partial 不重放

### Requirement: 能力探测与版本化

`GET /v1/capabilities` SHALL 返回 AgentAPI 能力集（`sessions.list`、`sessions.cancel`、
`decisions`、`partials`、`plugins`、`subagents`、`triggers`、`hooks` 等）与服务版本；
AgentAPI 以 `/v1` 前缀版本化，新增能力只增不改。

#### Scenario: 客户端按能力降级
- **WHEN** 客户端发现 `partials=false`
- **THEN** UI 关闭 token 级流式，只显示完整消息

### Requirement: 扩展面 API

AgentAPI SHALL 暴露扩展面的查询/控制面：`GET /v1/plugins`（已装配插件与 hooks/triggers 声明）、
`GET /v1/subagents`（子会话列表/状态）、`GET /v1/triggers`、`POST /v1/triggers/{id}/fire`
（手动触发，幂等键）。写操作 MUST 走认证中间件与租户隔离。

#### Scenario: 手动触发
- **WHEN** 调用 `POST /v1/triggers/{id}/fire` 带幂等键
- **THEN** 创建/唤醒对应会话并开始一个 turn；重复键不重复触发
