# external-gateway

## ADDED Requirements

### Requirement: 协议边界

外部服务 SHALL 通过 MCP 风格 JSON-RPC 接入，传输为 stdio 或 Streamable HTTP。
协议 MUST 支持初始化/能力协商、请求关联、取消、超时和错误返回；
传输/帧复用 MCP，但方法语义由本协议版本定义，MUST NOT 伪造为 MCP 标准方法。

#### Scenario: 初始化
- **WHEN** 外部进程启动并连接
- **THEN** 双方交换协议版本、能力集、服务标识；不兼容版本拒绝激活

### Requirement: AgentBackend 方法集

AgentBackend SHALL 是版本化的私有 JSON-RPC 方法集（复用 MCP 传输/帧）：
`initialize`、`session.open`、`session.resume`、`message.send`、`event.stream`、
`cancel`、`health`、`close`。初始化 MUST 协商协议版本与能力；不兼容版本拒绝激活。

#### Scenario: 版本不兼容
- **WHEN** backend 只支持 v0，而 Keel 要求 v1
- **THEN** 初始化失败，provider 不激活，错误可读

### Requirement: 外部会话数据模型

外部 backend 会话 MUST 落库标记：`state_owner=external`、`external_backend`、
`external_ref`、`external_status`。外部会话 MUST NOT 进入原生 run/lease 作业流；
SSE 只回放 Keel 记录的事件；backend 不可用 MUST 是可查询终态。

#### Scenario: backend 崩溃
- **WHEN** DSH sidecar 崩溃
- **THEN** 会话状态变为 backend-unavailable；API/审计可查；不声称原生恢复成功

### Requirement: 外部用量计量边界

外部 backend 的模型调用若 Keel 不可见，usage MUST 标记 `metering=external|none`；
MUST NOT 静默记 0 或伪装为原生 provider 用量。配额策略必须在 loadout/租户配置中显式选择。

#### Scenario: 无用量回报
- **WHEN** DSH backend 不返回 token 用量
- **THEN** 该会话 usage 标记 none/external；账单/报表显示不可计量，而不是 0

### Requirement: 两类外部服务

网关 SHALL 支持两类外部服务：

- `ToolProvider`：无状态工具，输入/输出 JSON；
- `AgentBackend`：整个外部 agent runtime（如 DSH），由 backend 自己拥有会话状态。

外部服务 MUST NOT 获得事件账本、lease、租户存储或内核管理接口。

#### Scenario: 外部工具调用
- **WHEN** `pi-loop` 调用外部 ToolProvider 的工具
- **THEN** 网关转发参数，结果由内核 `Commit` 记账；外部进程不直接写事件

### Requirement: 流式 JSON 与背压

网关 MUST 使用流式 JSON 解析，禁止把整个请求/响应帧读入内存。
帧 MUST 有大小上限；下游慢时 MUST 对上游施加背压，不能无限缓冲。

#### Scenario: 大参数
- **WHEN** 工具参数超过单块缓冲但低于帧上限
- **THEN** 解析器逐块消费，峰值内存有界

#### Scenario: 慢消费者
- **WHEN** 外部进程处理慢，内部调用方持续产出
- **THEN** 背压传到调用方，队列不超过配置上限；不得 OOM

### Requirement: 取消与超时

每个外部请求 MUST 带超时和可取消 context。取消 MUST 传递给外部进程；
超时后 MUST 返回确定错误，不得让 run 永久挂起。

#### Scenario: 外部进程卡死
- **WHEN** tool call 超过 timeout
- **THEN** 请求取消，返回 timeout 结果；run 可继续或按策略失败

### Requirement: 故障隔离

外部进程崩溃、坏帧、协议错误 MUST NOT 拖垮 Keel 进程或污染事件账本。
网关 MUST 标记该 provider unhealthy，并按策略重连/降级。

#### Scenario: 坏帧
- **WHEN** 外部进程发送不完整/非法 JSON
- **THEN** 该请求失败并记录诊断；其他 provider 与 run 不受影响

### Requirement: 凭据与审计

外部服务的密钥 MUST 只从租户配置/密钥引用解析；日志 MUST 脱敏。
每次外部调用 SHALL 记录 provider、tool/agent、耗时、结果状态；
MUST NOT 记录完整参数/消息正文。

#### Scenario: 日志检查
- **WHEN** 外部工具调用包含 secret
- **THEN** 日志只出现引用名/哈希，不出现明文

### Requirement: 兼容模式标记

AgentBackend 的会话 SHALL 在事件/元数据中标记 `external_backend` 与
`state_owner=external`；监控与 UI 必须能区分原生会话与兼容会话。

#### Scenario: 查询会话
- **WHEN** 查询一个 DSH backend 会话
- **THEN** 返回兼容模式标记，不声称具备 Keel 原生恢复保证
