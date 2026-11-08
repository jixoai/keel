# kernel-loop

## ADDED Requirements

### Requirement: 内核纯度

内核包（`internal/loop`、`internal/agentcore`、`internal/agenttool`、`internal/kernel`、
`internal/agent`）MUST NOT import 产品层（`internal/agentapi`、`internal/agentext`）或客户端
（`web/`、`packages/`）；依赖方向只允许产品 → 内核。包级测试 MUST 用 `go list -deps` 钉住。

#### Scenario: 产品依赖不进内核
- **WHEN** 有人在内核包里 import 产品层
- **THEN** `go test ./internal/...` 的边界测试失败并指出包名

### Requirement: 两种投递模式

`user.message.payload` MAY 携带 `delivery: "queue" | "guide"`（缺省 `queue`；其它值 400）。
语义与 PI `followUp/steer`、zcode `queue/guide` 对齐：

- `queue`：当前 run 结束后、session ready 边界才进入模型上下文；
- `guide`：当前 turn 内、**所有 sibling tool results 提交后**由 loop 的 steering 注入，
  一次最多一条；MUST NOT 打断 in-flight 模型流；非 streaming、有 stop 请求或有附件时
  MUST 降级为 `queue`（降级事实在事件里可见）。

#### Scenario: 工具轮边界注入
- **GIVEN** 一个 turn 有 3 个并行工具调用，guide 在它们完成前提交
- **WHEN** 3 个结果全部提交
- **THEN** 下一次 provider 请求包含该 guide；只完成 2/3 时不注入

#### Scenario: 降级
- **GIVEN** 会话没有在途流
- **WHEN** 客户端发 `delivery=guide`
- **THEN** 按 queue 处理，事件里 `delivery=queue`

### Requirement: 输入消费事实

`assistant.message.payload` SHALL 携带 `inputSeqs`（本次模型请求呈现给模型的 user 输入 seq，
升序）。「未答输入」MUST 由该集合判定（某 `user.message` 未被任何 assistant 的 `inputSeqs`
引用即为未答）。`read_watermark` MAY 保留为诊断/兼容，但 MUST NOT 作为唯一真值。

#### Scenario: assistant 已提交后崩溃
- **GIVEN** 回复已提交（含 inputSeqs），Finish 前进程被 kill
- **THEN** 接班者不重答该输入

#### Scenario: assistant 未提交时崩溃
- **GIVEN** 模型流中途 kill，未提交 assistant
- **WHEN** 接班者继续
- **THEN** 该输入仍为未答并被回答；每个输入恰好一条回答

#### Scenario: queue 与 guide 混排
- **GIVEN** 运行中先来 queue 再来 guide
- **THEN** guide 在当前 turn 消费、queue 在下一 turn 消费；各恰好一次

### Requirement: 运行摘要

run 收口（`done|failed|cancelled`）时 worker SHALL 提交 `run.finished`
（`turns`、`tokens`、`toolTimings`、`context`、`stopReason`、`durationMs`）。
该事件 MUST NOT 进入模型上下文；MUST NOT 因摘要失败改变 run 终态。

#### Scenario: 失败也摘要
- **WHEN** run 因 provider 401 失败
- **THEN** 日志有一条 `run.finished`（stopReason=error），上下文不受影响

### Requirement: loop 可替换（PI 契约不变）

内核 loop SHALL 保持 PI 的契约与命名（`steer()/followUp()`、`SteeringMode/FollowUpMode`、
三个 drain 点）；替换实现 MUST 通过既有 PI conformance 套件（fail-closed 的 manifest 哈希门）。
产品层 MUST NOT 依赖某个具体 loop 实现的内部结构。

#### Scenario: 替换 loop
- **WHEN** 用测试 loop 替换默认 pi-loop
- **THEN** conformance 门按 manifest 校验；未声明或漂移的实现被拒绝
