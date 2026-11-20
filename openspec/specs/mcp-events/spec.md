# mcp-events Specification

## Purpose

Keel 对 MCP 服务器通知与 modern `subscriptions/listen` 的处置契约：瞬态 progress/log
不进账本、目录变更驱动固定基数的工具面 republish、断线按退避重连并全量重算、
listen 与产品 SSE 隔离，以及 MRTR（`input_required`）、legacy 主动请求、era 探测与
降级诊断。目标是热更新可预期、失败保留旧面、账本零新增 kind。

## Requirements

### Requirement: 瞬态通知：进度与日志

`notifications/progress` MUST 只作瞬态：建议性、可丢帧，MUST 经请求侧进度 token 关联回
`(租户, 会话, 工具调用)` 且 MUST NOT 串会话；`notifications/message` MUST 只进结构化日志与指标
（`level` 取闭集 debug/info/warning/error，未知按 info；字段有界截断）。
两者 MUST NOT 写入事件账本、MUST NOT 进入会话时间线；丢帧 MUST NOT 影响工具调用正确性，
也 MUST NOT 让生产端阻塞。

#### Scenario: 进度不进账本
- **WHEN** 服务器对一个工具调用连发 50 条 `notifications/progress`
- **THEN** 账本中该调用只有一个 `tool.result`；进度只出现在瞬态通道/指标；重启后不必恢复进度

#### Scenario: 关联正确
- **WHEN** 两个会话各有一个在途工具调用，服务器分别发进度
- **THEN** 每条进度只出现在对应会话的通道；跨会话零串扰（断言）

#### Scenario: 服务器日志不进时间线
- **WHEN** 服务器发 `notifications/message {level:"error"}`
- **THEN** 结构化日志出现该条（字段有界、无正文泄漏），会话时间线与账本无任何新增

### Requirement: 目录通知与取消通知的处置

`notifications/{tools,prompts,resources}/list_changed` 与 `notifications/resources/updated`
MUST 触发注册面/目录重算，MUST NOT 写账本。
`notifications/cancelled` MUST 中止对应在途请求，其后果 MUST 以该调用的 `tool.result`（错误）落账本。
`notifications/subscriptions/acknowledged` MUST 更新订阅状态；
请求未被接受的类别 MUST 记诊断（MUST NOT 静默忽略）。

#### Scenario: 取消的最终事实在账本
- **WHEN** 服务器发 `notifications/cancelled` 取消我们的 `tools/call`
- **THEN** 该调用以一个错误 `tool.result` 落账本；随后重试是新的请求（新 id）

#### Scenario: ack 缺项有诊断
- **WHEN** 宿主请求 `promptsListChanged` 但 ack 的子集不含它
- **THEN** 宿主记录「该类别未获支持」诊断，并停止期待该类通知（不静默）

### Requirement: 账本零新增 kind

账本 MUST NOT 为本能力新增事件 kind：服务器的目录、日志、进度与订阅状态都不是会话事实，
也没有会话作用域。可变可回放的事实只有既有的两处通路：工具调用的 `tool.result`
与被 MRTR 转成问句的 decision 记录。

#### Scenario: 通知洪峰后账本不变
- **WHEN** 服务器连续发出目录变更、资源更新、日志与进度（含高频）
- **THEN** 会话账本的 kind 集合不新增；除该调用的 `tool.result` 外无新增事实

#### Scenario: 重启后无需恢复通知
- **WHEN** 进程重启后重新连接服务器
- **THEN** 宿主只重算目录（不尝试恢复通知）；账本回放不受影响

### Requirement: 订阅主体与 modern listen 契约

宿主 MUST 按**服务器**订阅（每连接一个订阅者），MUST NOT 为每会话/每客户端建订阅。
modern era MUST 用 `subscriptions/listen`，请求 MUST 显式列出订阅类型
（`toolsListChanged`/`promptsListChanged`/`resourcesListChanged`，`resourceSubscriptions` 按需最小化），
MUST NOT 期待未 opt-in 的通知；MUST 把首个消息 `notifications/subscriptions/acknowledged`
视为订阅成立，并按 `io.modelcontextprotocol/subscriptionId` 去复用通知。

#### Scenario: 首消息必须是 ack
- **WHEN** 宿主发起 `subscriptions/listen`
- **THEN** 在收到 ack 之前宿主不把该流消息当作变更通知处理；ack 缺失子集写入诊断

#### Scenario: 未 opt-in 的通知被忽略
- **WHEN** 宿主只 opt-in `toolsListChanged`，服务器在流上发 `notifications/resources/updated`
- **THEN** 宿主不处理该通知（记协议诊断），不触发目录重算

### Requirement: 断线重建与全量重算

modern 断流后宿主 MUST 重发 `subscriptions/listen`（服务端不留订阅状态），
MUST NOT 发送 `Last-Event-ID`、MUST NOT 期待事件重投递。
legacy MAY 继续使用既有 GET 流并按既有退避重连（仅在对端给过 event id 时携带 `Last-Event-ID`）。
两代在订阅重建成功后 MUST 全量重算一次目录（断线期间的变化没有回放）。

#### Scenario: 断流重订阅并重算
- **WHEN** listen 流中途断开后重连成功
- **THEN** 宿主重发 listen 且全量重算目录；断言没有依赖「重连补偿」的代码路径

#### Scenario: legacy 重连兼容
- **WHEN** 对端是只讲 2025-11-25 的服务端且 GET 流断开
- **THEN** 按既有退避重连（行为与今天逐字一致），不尝试 `subscriptions/listen`

### Requirement: 与产品 SSE 的隔离

本能力 MUST NOT 与产品 SSE（`/v1/sessions/{id}/events/stream`）共用连接、handler 或恢复语义：
前者作用域为服务器、真值源为服务器状态、modern 无恢复；后者作用域为会话、真值源为账本、
以 `Last-Event-ID` 续传。

#### Scenario: 代码路径隔离
- **WHEN** 审查/测试两条流
- **THEN** MCP 订阅实现不 import 产品 SSE 的 handler/hub，恢复语义各自独立（结构断言）

### Requirement: 目录变更驱动全量 republish 与 generation

`notifications/tools/list_changed` MUST 触发注册面重算，并以全量发布（republish）语义
一次更新工具面、资源目录、app 目录与相关提示段，MUST NOT 出现半面发布。
发布 MUST 递增一个 generation 并 MUST 通过可查询的变更身份暴露
（`/v1/mcp/servers` 与 `/v1/capabilities`），使客户端无需轮询全量即可发现变化。

#### Scenario: generation 可发现
- **WHEN** 任一服务器的目录发生变化
- **THEN** generation 严格递增；两次 `GET /v1/mcp/servers` 可区分「变了/没变」

#### Scenario: 无半面发布
- **WHEN** 插件目录刷新与 MCP 目录刷新接连发生
- **THEN** 两次发布后 MCP 工具与插件工具都在场（组合发布断言）

### Requirement: 热更的 attempt 边界与失败保面

热更新的生效边界 MUST 是**下一次 attempt**：进行中的 run MUST 用它开始时冻结的工具集继续执行，
MUST NOT 被中途替换或打断；宿主 MAY 发 hook 事件 `mcp.surface.changed`，
但 MUST NOT 据此改变运行中 run 的行为。重算失败（服务器断连/列表错误）MUST 保留上一版可用面，
MUST NOT 发布空面或半面；错误 MUST 在 `/v1/mcp/servers` 可见并写审计。

#### Scenario: 运行中变更不影响当前 run
- **WHEN** 一个 run 正在第 2 个 attempt，服务器发 `tools/list_changed` 新增/删除工具
- **THEN** 当前 attempt 的工具集与调用结果不变；下一个 attempt 看到新工具集；
  旧工具名在新工具集下不可调用（unknown tool）

#### Scenario: 失败保面
- **WHEN** `tools/list_changed` 后重算因服务器断连失败
- **THEN** 工具面保持上一版，调用继续可用；`/v1/mcp/servers` 报该服务器错误；审计可见

### Requirement: MRTR：resultType 与 inputRequests 执行

modern era 宿主 MUST 处理 `InputRequiredResult`：`resultType` 缺失 MUST 视为 `"complete"`，
出现未知值 MUST 视为协议错误（该调用以失败 `tool.result` 结束并审计）。
`"input_required"` 时 MUST 逐条执行 `inputRequests`：`elicitation/create` MUST 走
decision/consent 原语（用户可见、可超时、幂等；URL 模式 MUST NOT 自动打开链接）；
`sampling/createMessage` MAY 用当前会话模型执行，但 MUST 受开关与预算限制
（默认仅会话内、`maxTokens` 上限、每调用次数预算），只做客观用量计量、MUST NOT 写价格；
`roots/list` MUST 拒绝；未知方法 MUST 拒绝（`-32601`）并审计，MUST NOT 静默省略。

#### Scenario: 澄清一轮成功
- **WHEN** 服务器对 `tools/call` 回 `InputRequiredResult{inputRequests:{login:{…}}, requestState:"…"}`
- **THEN** 用户看到决定卡并作答；宿主带 `inputResponses` 与原样 `requestState` 以新 id 重试；
  成功结果落账本为一次 `tool.result`

#### Scenario: roots 拒绝
- **WHEN** `inputRequests` 含 `roots/list`
- **THEN** 宿主对该条回拒绝，整轮按策略失败/降级，审计可见；MUST NOT 伪造 roots

#### Scenario: 未知方法拒绝
- **WHEN** `inputRequests` 含一个未识别的 method
- **THEN** 该条回 `-32601` 并审计；MUST NOT 直接省略导致服务端误读

### Requirement: MRTR 重试、requestState 与预算

重试 MUST 使用**新的 JSON-RPC id**，且 MUST 原样回显 `requestState`
（MUST NOT 解析、改写或在其它请求上复用）。同一调用的 MRTR 往返 MUST 有轮次预算（默认 5），
超出 MUST 以失败 `tool.result` 结束并审计；每轮 MUST 受调用超时约束；
用户不答 MUST 以 `ElicitResult{action:"cancel"}` 结束。

#### Scenario: requestState 被篡改由服务端负责
- **WHEN** 宿主回显的 `requestState` 被服务端拒绝（完整性校验失败）
- **THEN** 该调用以失败 `tool.result` 结束；宿主不重试同一 state、不解析其内容

#### Scenario: 轮次超预算
- **WHEN** 服务器连续 6 轮都回 `input_required`
- **THEN** 第 6 轮前宿主以失败 `tool.result` 结束并写审计（避免无限往复）

#### Scenario: 用户不答
- **WHEN** 决定卡超时
- **THEN** 宿主以 `ElicitResult{action:"cancel"}` 结束该轮，并按预算决定是否继续

### Requirement: legacy 主动请求与能力声明一致

legacy era 的服务端主动请求（`sampling/createMessage`/`elicitation/create`/`roots/list`）
MUST 走同一执行器并返回对应响应。宿主 MUST 只在真正能处理时于 `ClientCapabilities` 声明
`sampling`/`elicitation`：声明与实现一致，MUST NOT 声明后回 `method not found`。

#### Scenario: legacy sampling 得到结果
- **WHEN** legacy 服务端在工具执行中发 `sampling/createMessage`
- **THEN** 宿主按预算执行并返回 `CreateMessageResult`；审计记录该次采样

#### Scenario: 未实现则不声明
- **WHEN** 某部署关闭了 elicitation 处理
- **THEN** `initialize` 的 `ClientCapabilities` 不含 `elicitation`；服务端 MUST NOT 发该类请求，
  真收到时宿主回 `-32601` 且审计

### Requirement: era 支持与 auto 探测

宿主 MUST 同时支持 legacy（≤2025-11-25，现行为，PI 基线）与 modern（2026-07-28），
以版本门控隔离差异；era 策略 MUST 可配置（`auto`/`legacy`/`modern`，默认 `auto`），
且默认 era 下 PI 一致性套件 MUST 全绿。`auto` MUST 遵循 spec 的兼容算法：先以现代形态请求；
若响应为 400 且 body 是可识别的现代 JSON-RPC 错误 → 判定 modern（按服务端 advertised 版本重试）；
若 body 空或不可识别 → 回落 `initialize`（legacy 全程）。

#### Scenario: 现代服务端免握手
- **WHEN** 对端只讲 2026-07-28（拒绝 initialize、要求每请求 `_meta`）
- **THEN** 探测判定 modern；后续请求无 session 头、无 GET 流，`server/discover` 的信息
  出现在 `/v1/mcp/servers`

#### Scenario: 老服务端不受影响
- **WHEN** 对端是只讲 2025-11-25 的服务端
- **THEN** 探测回落 legacy；`initialize` 握手、session、GET 流、`Last-Event-ID` 恢复行为
  与今天逐字一致（一致性套件断言）

### Requirement: modern 传输契约

modern 形态下宿主 MUST：不发也不接受 `Mcp-Session-Id`（不 DELETE session）、不发 `Last-Event-ID`；
每个请求 body 携带 `io.modelcontextprotocol/protocolVersion` 与
`io.modelcontextprotocol/clientCapabilities`（`clientInfo` SHOULD 携带）；
HTTP 请求 MUST 携带 `MCP-Protocol-Version`（与 `_meta` 一致）、`Mcp-Method` 与适用的 `Mcp-Name`
（非 ASCII 按 spec 的 base64 哨兵编码）；MUST 容忍 GET/DELETE 返回 405；
MUST 解析并使用 `ttlMs`/`cacheScope`（仅 `cacheScope:"public"` 允许跨租户共享缓存）。

#### Scenario: 头与 _meta 必须一致
- **WHEN** 宿主构造 modern 请求
- **THEN** `MCP-Protocol-Version` 与 body `_meta` 版本一致、`Mcp-Method` 与 body `method` 一致
  （假服务端对错配回 400，测试断言宿主不产生错配）

#### Scenario: 缓存作用域
- **WHEN** 一个 `resources/read` 结果 `cacheScope:"private"` 或缺失
- **THEN** 宿主只在本租户内缓存该项；不跨租户共享

### Requirement: era 可观测

`GET /v1/mcp/servers` MUST 报告每个服务器的 era、协商到的协议版本与能力集
（含 `experimental`/`extensions`），使运维与 UI 能分辨「对端讲哪一代」。

#### Scenario: 状态可查
- **WHEN** 连接一个 modern 与一个 legacy 服务端
- **THEN** 一次请求即可分辨两者的 era 与版本；UI 无需另开接口

### Requirement: 降级、诊断与可观测

任何一步失败（订阅失败、通知解析失败、目录重算失败、MRTR 超时/拒绝、版本探测失败）
MUST 有明确降级：MCP 工具面保持上一版可用、会话与 run 不受影响、错误可查；
MUST NOT 静默吞掉（诊断进日志与审计）。

宿主 MUST 暴露固定基数的指标（与会话数无关）：服务器状态、注册面 generation、文档取回结果、
桥帧计数、订阅重连次数、MRTR 轮次分布；结构化日志 MUST 含 server/调用标识，
MUST NOT 含消息正文或凭据。

#### Scenario: 订阅失败不影响工具
- **WHEN** 服务器拒绝 `subscriptions/listen`（回错误）
- **THEN** 工具调用照常可用（目录不刷新）；诊断与审计可见；重连按退避重试而不是疯转

#### Scenario: 指标基数有界
- **WHEN** 在 1000 会话、20 个 app 实例下抓取指标
- **THEN** 指标 series 数不随会话数增长（标签只用 server/method/result 这类有界维度）

#### Scenario: 日志无正文
- **WHEN** 一个工具调用失败并把服务器错误文本带回
- **THEN** 结构化日志与审计只含标识、错误类别与有界摘要；正文只出现在对用户可见的会话结果里
