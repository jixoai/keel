# runtime-handoff

## ADDED Requirements

### Requirement: 新 runtime 接班协议

原生会话的升级 SHALL 通过 runtime handoff 完成，而不是进程内热替换代码：

1. 新 runtime 以新 loadout 启动并完成 ready；
2. 旧 runtime 停止领取新 run；
3. 旧 runtime 排空在途 run，写入所有已提交事实；
4. 旧 runtime 释放 lease；
5. 新 runtime 领取会话并获取新 epoch；
6. 从事件日志重建，继续 SSE 游标。

#### Scenario: 正常交接
- **WHEN** 新 runtime 启动并进入 ready，旧 runtime 收到 drain
- **THEN** 旧 runtime 不再 claim；完成在途 run 后释放 lease；新 runtime 接手

### Requirement: fencing 拒绝旧 writer

交接后，旧 runtime MUST 无法用旧 epoch 提交 owner-scoped 事件。edge-scoped 的
user.message 仍可落账，但必须由新 owner 处理。客户端看到的事实只来自已提交事件。

#### Scenario: 旧 writer 晚到
- **GIVEN** 新 runtime 已取得 epoch+1
- **WHEN** 旧 runtime 提交旧 epoch 的 assistant 事件
- **THEN** 内核拒绝；旧 runtime 不得重试写入，错误进入审计

### Requirement: 五类安全点

handoff 测试 MUST 覆盖：用户消息提交后、assistant tool call 提交后、
tool result 提交后、模型流中、工具执行中。每类都必须满足：不丢事件、
不重复最终回复、run 最终收敛。

#### Scenario: assistant tool call 后交接
- **WHEN** 旧 runtime 已提交 tool call、结果尚未提交时被 drain
- **THEN** 新 runtime 重新执行缺失工具（at-least-once），最终一条 tool.result

#### Scenario: 模型流中交接
- **WHEN** 模型流进行中旧 runtime 被停止
- **THEN** 未提交 partial 丢弃；新 runtime 从最后提交点继续，不出现半条消息

### Requirement: SSE 游标连续

客户端重连 MUST 从 `Last-Event-ID` / offset 继续；handoff 不得制造 seq 缺口。
Keel SSE 只发送已提交事件；已提交消息 MUST 可重放。

#### Scenario: 客户端跨 handoff 重连
- **WHEN** 客户端在旧 runtime drain 时断开，随后连到新 runtime
- **THEN** 首帧为游标+1，已提交消息不重复、不缺失

### Requirement: runtime 身份与 drain 截止

系统 SHALL 持久化 runtime_id，并写入 claim/审计。drain MUST 有截止时间；
截止后旧 owner 的 lease 可被强制回收，但必须记录超时原因。handoff 期间
双进程 RSS MUST 纳入 2GB 验收。

#### Scenario: 旧 runtime 不排水
- **WHEN** drain 超过截止时间，旧 runtime 仍有在途 run
- **THEN** 新 runtime 可按 lease 过期强制接管；旧 owner 的提交被 fencing 拒绝；审计记录超时

### Requirement: SSE 读路径归属

SSE 读 MUST 只读事件日志，不依赖 owner；但 handoff 验收 MUST 明确客户端最终连接到
新 runtime。旧 runtime 继续服务旧连接 MAY 存在，但不得改变“新 owner 负责执行”的事实。

#### Scenario: 旧连接继续读
- **WHEN** 客户端仍连着旧 runtime 的 SSE
- **THEN** 它只能读到已提交事件；执行与后续输入由新 owner 负责，客户端可重连切换

### Requirement: loadout 版本固定

每个 run SHALL 记录它使用的 loadout revision 与 owner epoch；handoff 审计
MUST 能回答「哪个 run 在哪个 revision/epoch 上完成」。

#### Scenario: 交接后审计
- **WHEN** 查询一次 handoff
- **THEN** 返回旧/新 runtime、loadout revision、epoch、开始/结束时间、在途 run

### Requirement: 两种 drain 模式

系统 SHALL 区分：

- `shutdown`：SIGTERM/部署停机；停止 claim、拒绝新写、按现有 ops 语义关闭 SSE；在途 run 可放弃，lease 过期后由新进程恢复；
- `handoff`：升级/换 loadout；停止 claim，但继续接受 edge-scoped 输入；在途 run 到安全点或截止后停止；新 runtime 接班。

两种模式 MUST 在日志/审计中标记；handoff 模式 MUST NOT 复用 shutdown 的“拒绝输入”分支。

#### Scenario: SIGTERM shutdown
- **WHEN** 进程收到 SIGTERM
- **THEN** 按 shutdown 模式排水；新输入可被 503，客户端重试到新进程

#### Scenario: handoff drain
- **WHEN** 运维触发 handoff
- **THEN** 停止 claim、继续收 user.message、在途 run 排水，新 runtime 接手

### Requirement: 外部 backend 的兼容模式

通过 ExternalAgentBackend 接入的 DSH 会话 SHALL 作为兼容模式单独标记：
其会话状态由 backend 拥有，Keel 不承诺原生的 kill-9 重建/交接保证。

#### Scenario: DSH backend 崩溃
- **WHEN** DSH sidecar 崩溃
- **THEN** Keel 标记该外部会话为 backend-unavailable；不伪造原生恢复成功

### Requirement: 排水不丢新输入

交接（handoff）模式下新到的 user 消息 MUST 继续追加到事件日志，并在新 runtime 接手后处理；
旧 runtime MUST NOT 丢弃已接收的输入。SIGTERM 的 shutdown 模式保留现有语义：
拒绝新写请求，客户端重试/重连到新 runtime。两种模式 MUST 在配置/审计中可区分。

#### Scenario: drain 期间用户继续发消息
- **WHEN** 旧 runtime 已停止 claim，但客户端提交新 user.message
- **THEN** 事件照常落账；新 runtime 领取 pending run 后处理
