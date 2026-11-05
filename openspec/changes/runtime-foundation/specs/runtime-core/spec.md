# runtime-core

## ADDED Requirements

### Requirement: 事件日志是唯一真值

系统 SHALL 把会话的每一次状态变化记录为不可变事件行（append-only），并以单调递增的
`offset` 排序。任何对外可见的内容 MUST 在事务提交后才可读；未提交的部分 MUST NOT 展示。

#### Scenario: 提交后可见
- **WHEN** 一次模型流式输出被提交
- **THEN** 订阅者收到的事件 offset 严格递增，且不含未提交的 partial 内容

#### Scenario: 进程死亡不污染日志
- **WHEN** 进程在事务提交前 `kill -9`
- **THEN** 重启后日志中不存在半条事件

### Requirement: 检查点是投影，不是真值

系统 SHALL 支持将事件日志折叠为 checkpoint 以加速恢复；checkpoint MUST NOT 覆盖或删除
事件。恢复 MUST 为「最近 checkpoint + 其后的 tail events」。

#### Scenario: 压缩不改写历史
- **WHEN** 触发上下文压缩
- **THEN** 追加 `compaction` 事件，原始事件仍可按 offset 读回

### Requirement: 每个 await 点可重入

模型请求、工具执行、生成任务 SHALL 建模为持久任务；进程重启后 MUST 从最后一个已提交状态
继续，且结果与未中断时一致。

#### Scenario: 模型流中崩溃
- **GIVEN** 模型流已提交若干 partial 事件
- **WHEN** 进程被 `kill -9` 后重启
- **THEN** 该 attempt 记为失败（usage 落账），按退避重试，最终提交恰好一条最终回答

#### Scenario: 工具执行中崩溃
- **GIVEN** 工具已开始执行且未提交结果
- **WHEN** 进程被 `kill -9` 后重启
- **THEN** 该工具任务 `restarts ≥ 1` 并重新执行；框架 MUST NOT 声称「副作用恰一次」

### Requirement: 请求幂等

系统 SHALL 以 `(tenant_id, request_id)` 唯一约束保证提交幂等。相同 `requestId` 的重复提交
MUST 返回同一个 submission，MUST NOT 产生第二条 user 消息。

#### Scenario: 跨进程重投
- **WHEN** 客户端在进程重启后以同一 `requestId` 重发
- **THEN** 返回原 submissionId，会话条目不增

### Requirement: 零进程状态

除连接与缓存外，进程 MUST NOT 持有会话事实。任意时刻 `kill -9` 后重启，所有会话 MUST 可从
存储恢复。

#### Scenario: 重启后继续
- **WHEN** 进程重启并收到原会话的新输入
- **THEN** 模型看到的上下文与崩溃前已提交内容一致
