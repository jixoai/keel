# runtime-stream

## ADDED Requirements

### Requirement: SSE 断线续传

系统 SHALL 提供 SSE 事件流，事件带单调 `id`（offset）。客户端重连携带
`Last-Event-ID` 时，服务端 MUST 从该 offset 之后继续发送，不得重发已确认事件。

#### Scenario: 断线后的增量
- **GIVEN** 客户端已收到 offset 100
- **WHEN** 连接断开后重连并携带 `Last-Event-ID: 100`
- **THEN** 首帧 offset 为 101（或明确告知无新事件）

### Requirement: 缺口可检测可补齐

当客户端错过一段事件（如超出窗口），系统 MUST 通过 `before` 边界让客户端感知缺口；
分页接口 SHALL 支持按 `before` + `limit` 拉取更早事件。

#### Scenario: 补洞
- **GIVEN** 客户端本地缺失 offset 90–120
- **WHEN** 客户端请求 `before=121&limit=100`
- **THEN** 返回包含 90–120 的条目，且客户端合并后无重复、保序

### Requirement: 心跳与失活检测

长连接 SHALL 每 15s 发送注释心跳。服务端 SHALL 在写超时或进度超时后关闭连接，
不得无限占用 goroutine。

#### Scenario: 空闲连接
- **WHEN** 连接 45s 无业务事件
- **THEN** 客户端仍收到心跳，连接未被误杀

### Requirement: 多客户端一致

同一会话的多个订阅者 SHALL 收到相同顺序的事件；广播 MUST NOT 因某个慢客户端阻塞其他客户端。

#### Scenario: 一个慢客户端
- **WHEN** 某客户端读取缓慢
- **THEN** 它被单独取消，其他客户端继续正常接收
