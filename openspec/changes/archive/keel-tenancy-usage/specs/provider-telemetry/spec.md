# provider-telemetry

## ADDED Requirements

### Requirement: 请求级 telemetry 接缝

`provider.StreamConfig` SHALL 增加 `Telemetry telemetry.Context` 字段，作为可选接缝；
provider 实现 MAY 忽略它。请求级 span 的安装 MUST 通过包装 `StreamFn` 完成，MUST NOT
要求每个 provider 各自实现遥测。

#### Scenario: provider 不感知遥测
- **WHEN** 未安装包装器
- **THEN** 现有 provider 行为不变，`StreamConfig.Telemetry` 为空

### Requirement: pi.ai.request span 内容

系统 SHALL 提供 `agent.NewObservedStream(base provider.StreamFn, tel telemetry.Context) provider.StreamFn`，
为每次模型请求记录名为 `pi.ai.request` 的 span。属性 MUST 包含 provider、model、api
（上游实际返回时）、first_chunk（毫秒）、stop 与 usage.input/output/cache_read/
cache_write（上游实际返回时）。span MUST NOT 记录消息正文、工具参数、response id
或密钥。

#### Scenario: 成功请求
- **WHEN** 上游返回 start/text/done，usage 为 input=100/output=40/cache_read=30/cache_write=20
- **THEN** span 名为 pi.ai.request，状态 ok，属性与 usage 一致，first_chunk ≥ 0

### Requirement: span 生命周期与容错

span MUST 在调用 `base` 之前开始，因此连接/建流失败 MUST 也有 span（error 状态；
context canceled 记为 aborted/ok）。`tel` 为 nil 或 base 为 nil 时 MUST 原样返回
base，零额外开销。若 telemetry adapter 拒绝接纳 callback，包装器 MUST 关闭返回的
stream 并让 StreamFn 返回错误，避免消费者永久阻塞。

#### Scenario: 建流失败
- **WHEN** `base` 在返回 stream 前报错
- **THEN** 仍产生 pi.ai.request span（error，除 context canceled 外），且 StreamFn 返回该错误

#### Scenario: telemetry 拒绝 span
- **WHEN** StartSpan 在接纳 callback 前返回错误
- **THEN** 返回的 stream 被关闭并带错误，消费者不会阻塞

### Requirement: 请求级指标

系统 SHALL 在同一包装器中记录 `pi.ai.request.duration`（ms）、
`pi.ai.first_chunk.duration`（ms，出现首个内容增量时）与 `pi.ai.tokens`
（按 type=input/output/cache_read/cache_write 的计数）。指标属性 MUST 限定为
provider/model/api/stop/type 等低基数标签，MUST NOT 使用 session/run/response id。

#### Scenario: 指标落账
- **WHEN** 一次请求返回上述 usage
- **THEN** memory adapter 中恰有 request.duration 与 first_chunk.duration 直方图测量，
      以及四种非零 token 计数

### Requirement: 状态语义

错误 stop（stopReason=error 或带非 aborted 错误的 StreamErrorEvent）MUST 把 span
状态置为 error 并携带错误消息；aborted（用户取消/shutdown）MUST 保持 ok；
上游在无 terminal 事件时关闭 MUST 视为异常并置 error；context 取消导致停止转发
MUST 记为 aborted 且 ok。

#### Scenario: 错误与中止
- **WHEN** 上游错误事件 stopReason=error
- **THEN** span 状态 error，stop=error
- **WHEN** stopReason=aborted
- **THEN** span 状态 ok，stop=aborted
