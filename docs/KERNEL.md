# KERNEL — keel 内核边界

keel 的内核只有一个职责：**把一个会话的一次运行（run）跑成一个可重放的线性事件账本**。
它和 PI 一样是一个 loop，可以整体替换；产品能力（会话列表、停止、审批、插件、subagent、
触发器、hooks、UI）全部在内核之上，内核不感知「谁在用它」。

结构约束由测试钉住：`internal/architecture/boundary_test.go` 用 `go list -deps` 断言内核包
不得 import 产品层。违反即 `go test ./...` 失败。

## 1. 包边界

| 包 | 职责 | 不做什么 |
| --- | --- | --- |
| `internal/agentcore` | 消息/内容/事件类型（PI 的 message & event 语义） | 不读日志、不调用模型 |
| `internal/agenttool` | 工具执行与批次（并行调用、结果配对） | 不认识具体工具的业务含义 |
| `internal/loop` | 一次 loop 执行：turn、tool batch、steering/follow-up 三个 drain 点、compaction、settle | 不写账本（提交由宿主做） |
| `internal/agentloop` | PI loop 的替换契约与 conformance manifest（`pi` 实现 + 测试 loop） | 不绑定某个具体实现 |
| `internal/kernel` | 账本：事件提交/去重、lease/fencing、run 行、外部会话通道、投递模式与运行摘要的校验 | 不做业务决策（谁该跑、跑什么） |
| `internal/agent` | runner（rebuild → 填洞 → loop → 提交）＋ worker（claim/renew/settle） | 不做认证、列表、审批、插件 |

产品层（**只能依赖内核，不能被内核依赖**）：

- `internal/agentapi` —— `/v1` AgentAPI（会话列表/停止/审批/扩展面）
- `internal/agentext` —— 插件、触发器、subagent、hooks、RunControl、partials
- `internal/servicehost` —— 服务图装配（loadout / generation）
- `cmd/keel` —— `keel serve` / `keel tui`

## 2. 内核的公共契约面

### 事件账本

事件 kind 是唯一的 durable 契约（`internal/kernel/validate.go` 的 `kindRegistry`，
字符串与 `internal/store` 逐字对齐并由测试钉住）：

| kind | 类别 | 语义 |
| --- | --- | --- |
| `user.message` | edge | 客户端输入；`payload.delivery: queue\|guide` |
| `assistant.message` | owner | 模型回答；`payload.inputSeqs` 记录本次请求呈现给模型的输入 |
| `tool.result` | owner | 工具结果，按 `toolCallId` 与调用配对 |
| `compaction` | owner | 上下文压缩事实 |
| `run.finished` | owner | 一次 attempt 的摘要（status/turns/tokens/toolTimings/context/stopReason/error/durationMs） |
| `run.started` / `run.failed` | owner | 保留词汇（当前无运行时发射） |
| `external.event` | external | 外部 AgentBackend 的兼容日志（只进 external 会话） |

所有 owner 类提交都带 fencing token；`run.finished` 在 settle 之前用同一个 lease 提交，
摘要失败不改变 run 终态。转录重建（`internal/agent/messages.go`）忽略账本类事件。

### 投递模式（queue / guide）

- `queue`（缺省）：run 结束后、下一次 claim 时才进入模型上下文。
- `guide`：当前 turn 内注入，**在所有 sibling tool results 提交之后**，一次一条，不打断模型流。
  没有活跃 attempt 时由 HTTP 边界降级为 `queue`（事件里可见）。
- 实现：`internal/agent/steering.go`（从账本读未消费的 guide，作为 loop 的
  `GetSteeringMessages`）。注入的 seq 并入随后 assistant 的 `inputSeqs`——按**请求边界快照**
  （loop 的 `PrepareRequest`）归属，而不是提交时读实时状态：loop 会通过事件缓冲跑到
  runner 前面，读取实时状态会把「下一次请求才注入的 guide」记到上一次回答上。

### 输入消费事实（inputSeqs）

`assistant.message.payload.inputSeqs` = 这次请求呈现给模型的 `user.message` seq 集合。

- 「未答输入」＝没有被任何**完成态** assistant（`end_turn`/`tool_use`/…）的 `inputSeqs`
  引用的输入；`stopReason: error|aborted` 的终止消息不算回答（失败会重试，取消不重跑）。
  终止消息也**不进模型上下文**（它是失败尝试的记录，重放空/错误 assistant 会污染下一次请求）；
  最新 assistant 是终止失败时，`owedTurn` 为真——否则「工具轮完成 + 下一轮 provider 失败」
  重试时会以为无事可做，把 run 落成 done。
- `read_watermark` 保留为诊断/兼容与**老日志/升级前缀**的判定：新日志以 `inputSeqs` 为准
  （`owedTurn`）；第一条带 `inputSeqs` 的 assistant 之前的输入仍按水位决定（不再重答，
  即使那条边界消息是终止失败），从它开始的输入由 `inputSeqs` 判定，仍是「呈现过但没消费」。
- kernel 的 settle 也用它判定是否 re-queue（PG: `payload->'inputSeqs' @> seq`，且排除
  `stopReason in (error, aborted)` 的终止消息；MemStore 逐条等价）。
- kernel 的 settle 也用它判定是否 re-queue（PG: `payload->'inputSeqs' @> seq`）。

### 运行摘要

worker 在 settle 前提交 `run.finished`：`done|failed|cancelled` + turns/tokens/
toolTimings/context/stopReason/error/durationMs。provider 失败按 PI 语义是
`stopReason: error` 的终止消息，runner 把它归类为 attempt failed，worker 以失败 settle
（重试策略照常）。取消（`FinishRequest.Cancelled`）是终态：不自动重跑。

## 3. 二次开发

| 想做的事 | 做法 |
| --- | --- |
| 换 loop 实现 | 实现 `internal/agentloop` 的契约，跑 conformance manifest（fail-closed 哈希门） |
| 加工具 | `agenttool.ToolRegistry`（内核侧）或产品层 `ProductOptions.ExtraTools` |
| 加产品能力 | 在 `internal/agentext` 加插件/触发器/hook，经 `keel serve` 装配 |
| 加事件 kind | 走 openspec change：`kindRegistry` + `internal/store` 常量 + 转录/SSE/R1 规则同步 |

**不属于内核**：认证（网络中间件）、会话列表、停止/审批的语义、插件生命周期、触发器调度、
subagent 编排、token partials、任何 UI。
