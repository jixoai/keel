# contract-kernel

## ADDED Requirements

### Requirement: 事件只能经内核提交

所有会话事件 MUST 通过契约内核的 `Commit` 路径追加。服务图内的插件 MUST NOT
直接持有事件表写句柄、MUST NOT 自行分配 seq 或绕过租户/lease 校验。

`Commit` SHALL 按提交类别校验：owner-scoped 校验 tenant/session 归属、owner epoch、
幂等键、工具调用/结果配对；edge-scoped 只校验 tenant/session、白名单种类与幂等。
两类都 MUST 在同一个事务内追加，并共用同一 seq 锁。

#### Scenario: 插件尝试直接写事件
- **GIVEN** 一个服务插件拿到了运行时依赖
- **WHEN** 它尝试绕过 `Commit` 直接写 `events`
- **THEN** 依赖注入不提供原始 store 写句柄，提交被拒绝并记录诊断

### Requirement: 单调顺序与单写者

同一会话的事件 seq MUST 单调递增且无缺口。同一时刻同一会话最多一个有效
owner；owner 通过 lease + fencing token 表示。

#### Scenario: 两个 runtime 同时跑同一会话
- **WHEN** 旧 runtime 尚未释放 lease，新 runtime 试图领取
- **THEN** 只有一个 owner 获得有效 token；另一个必须等待过期或排水

### Requirement: 幂等键

系统 SHALL 支持三类幂等：`requestId`（客户端提交）、`generationId`（模型生成）、
`toolCallId`（工具结果）。相同键重复提交 MUST 返回已有事实，不得追加第二条。

#### Scenario: 客户端重投同一 requestId
- **WHEN** 同一 `requestId` 在第一次提交成功后再次到达
- **THEN** 返回第一次的 submission/seq，不重复 `user.message`

#### Scenario: 模型重试
- **WHEN** 同一 `generationId` 的 assistant 事件已提交，worker 又重试
- **THEN** 复用已有 assistant 事件，不再请求模型

### Requirement: 工具调用 ↔ 结果配对

assistant 消息中的每个 tool call MUST 有且只有一个最终 `tool.result`。
内核 MUST 拒绝孤儿结果、重复结果，以及在同一 turn 内把结果挂到错误调用上。

恢复时，已提交 tool call 但缺少结果 MUST 进入可重试状态；结果提交后 MUST
幂等收敛。

#### Scenario: 孤儿 tool result
- **WHEN** 服务提交一个 `tool.result`，对应的 `toolCallId` 不存在
- **THEN** `Commit` 拒绝该事件，账本不变

#### Scenario: 重复工具结果
- **WHEN** 同一 `toolCallId` 的结果被提交两次
- **THEN** 第二次返回已有 seq，不追加重复事件

### Requirement: 单一线性化点

`Claim` / `Renew` / `Commit` / `Finish` MUST 以 session 行（或与之同锁的 lease 行）
为唯一线性化点。owner-scoped `Commit` MUST 在持锁事务内重读 epoch；旧 epoch 一律拒绝。
所有事务 MUST 统一锁序 `sessions → runs`，禁止反向，避免 claim/finish 死锁。

#### Scenario: claim 与 stale commit 交错
- **GIVEN** 旧 writer 已读到 epoch=1，新 runtime 正在 claim
- **WHEN** 两者并发
- **THEN** claim 先拿 session 锁并递增 epoch；旧 writer 在持锁后重读发现 epoch 已变，提交被拒绝；不得出现两条最终回复

### Requirement: 提交类别

系统 SHALL 显式区分：

- owner-scoped：assistant.message、tool.result、compaction、run 生命周期；必须带 owner+epoch；
- edge-scoped：user.message；只校验 tenant/session、白名单、幂等；drain 期间仍可落账。

#### Scenario: drain 期间用户输入
- **WHEN** 旧 runtime 已停止 claim，客户端提交 user.message
- **THEN** edge-scoped Commit 接受并分配 seq；新 runtime 领取 pending run 后处理

#### Scenario: 非白名单 edge 事件
- **WHEN** 客户端向边缘端点提交 assistant.message
- **THEN** 400，账本不变

### Requirement: 边缘事件端点收紧

`POST /v1/sessions/{id}/events` SHALL 只接受客户端白名单（user.message）；
其他 kind 返回 400。跨租户/不存在会话 MUST 返回 404，而不是 200+空数组。
这是本 change 显式登记的 MODIFIED API 行为。

#### Scenario: 跨租户读事件
- **WHEN** tenant A 读取 tenant B 的 events
- **THEN** 404，且不泄露存在性

### Requirement: lease 与 fencing

`Claim` SHALL 返回 `owner` + `epoch`（fencing token）；`Renew`/`Commit`/`Finish`
MUST 携带当前 token。租约过期或 owner 变更后，旧 token 的提交 MUST 被拒绝。

#### Scenario: 交接后旧 runtime 晚提交
- **GIVEN** 新 runtime 已领取同一会话，epoch 递增
- **WHEN** 旧 runtime 用旧 epoch 提交 assistant 消息
- **THEN** 提交被拒绝；旧 runtime 必须丢弃本地结果并退出

### Requirement: 租户与配额内核执行

所有读写 MUST 带 tenant 边界；跨租户读取 MUST 表现为不存在。并发 run、流、
带宽、存储配额 MUST 在内核/边缘执行，插件不能自行放宽。

#### Scenario: 跨租户访问
- **WHEN** tenant A 的 token 读取 tenant B 的 session
- **THEN** 返回 404，且不泄露存在性

### Requirement: 崩溃恢复

重启后 MUST 仅从已提交事件 + 最近 checkpoint 重建；checkpoint MUST NOT 覆盖事件。
未提交的进程内状态 MUST 被丢弃。

#### Scenario: 模型流中 kill
- **WHEN** assistant 事件提交前进程被 kill
- **THEN** 重启后从最后已提交事件继续；不会出现半条消息

### Requirement: 服务只提交「提案」

服务（loop/prompt/tools/llm/compaction/mcp）SHALL 产生待提交结果或事件提案；
最终写入 MUST 由内核完成。服务 MUST NOT 直接改变 session 当前 surface/seq。

#### Scenario: 压缩插件失败
- **WHEN** 压缩服务生成摘要失败
- **THEN** 不提交 compaction 事件；原上下文保持可继续
