# mcp-apps Specification

> 能力：keel 作为 MCP **host** 渲染第三方服务器声明的交互界面（MCP Apps，SEP-1865，
> 扩展标识 `io.modelcontextprotocol/ui`；Stable 2026-01-26，延续进 2026-07-28 核心修订）。
> 设计依据：`openspec/changes/feat-mcp-apps-events/design.md` §0/§3。
> 术语：**View**＝被渲染的 app 页面（iframe/WebView）；**Host**＝keel；
> **绑定**＝「哪个工具调用的结果对应哪个 app」。

## ADDED Requirements

### Requirement: UI 资源的声明与读取

UI 资源 MUST 由工具 `_meta.ui.resourceUri` 声明（`ui://` scheme），
MUST 兼容废案平铺键 `_meta["ui/resourceUri"]`（两者并存时 `_meta.ui` 优先）。
工具未声明 `_meta.ui` 时其结果 MUST 与今天完全一致（纯文本）。
宿主 MUST 用 `resources/read` 取回内容并校验 `mimeType` 为 `text/html;profile=mcp-app`、
内容为 `text` 或 base64 `blob` 之一、大小有界（默认 2 MiB）；
资源的 `_meta.ui`（csp/permissions/domain/prefersBorder）MUST 被解析并随目录下发。

#### Scenario: 声明与取回
- **WHEN** 一个服务器的工具声明 `_meta.ui.resourceUri = "ui://weather/dashboard"`，且
  `resources/read` 返回 `text/html;profile=mcp-app` 的 HTML 与 `_meta.ui.csp`
- **THEN** 宿主目录出现该 app（含 appId、hash、csp、permissions），文档可按 hash 命名的 URL 取回

#### Scenario: 类型不符不渲染
- **WHEN** 服务器对 `ui://` 资源返回 `mimeType: "application/json"`
- **THEN** 该 app 不进可渲染目录，诊断记录「声明类型不符」，工具结果仍以文本呈现

#### Scenario: 废案键兼容
- **WHEN** 一个老服务器只在 `_meta["ui/resourceUri"]` 声明界面
- **THEN** 宿主仍能渲染该 app（记一条「已弃用写法」诊断），且不因两键并存而重复建 app

### Requirement: app 身份、缓存身份与目录

宿主 MUST 用派生身份标识 app（`appId = hash(server, resourceUri)`），
MUST NOT 把服务器提供的字符串直接用作 URL 或缓存键。
文档内容身份 MUST 为内容哈希，内容变更 MUST 产生新的不可变缓存 URL，
MUST NOT 让客户端把旧缓存当作新文档。app 目录 MUST 含 server、tool、resourceUri、visibility、
csp、permissions、prefersBorder、hash 与文档 URL；目录是机器可读的单一来源，
客户端 MUST NOT 自行重算绑定。

#### Scenario: 内容变更换 URL
- **WHEN** 同一 `ui://` 资源的 HTML 被服务器更新
- **THEN** 文档 hash 与 URL 随之变化，旧 URL 的不可变缓存不影响新内容

#### Scenario: 超限不渲染
- **WHEN** 资源内容超过 2 MiB
- **THEN** 该 app 不被渲染（诊断 + 审计），工具结果仍以文本兜底

### Requirement: app 文档托管路由与安全头

宿主 MUST 提供 app 文档托管路由，且 MUST 只服务已注册的 appId（未知 appId 404）。
响应 MUST 带 `X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer`、
按资源元数据构造的 `Content-Security-Policy` 与 `frame-ancestors 'self'`；
缓存 MUST 以内容 hash 为身份（不可变缓存），MUST NOT 让中间层缓存其它可变内容。

#### Scenario: 未知 appId
- **WHEN** 客户端请求一个未注册的 appId 文档
- **THEN** 404；不返回任何占位页面，也不泄露服务器清单

#### Scenario: 安全头断言
- **WHEN** 自动化 E2E 请求 app 文档
- **THEN** 断言四个头都在场；缺失即测试失败

### Requirement: CSP 构造：只收紧不放宽

CSP MUST 只由资源 `_meta.ui.csp` 构造：`default-src 'none'`；
`script-src`/`style-src` 为 `'self' 'unsafe-inline'` + `resourceDomains`；
`img-src`/`media-src` 为 `'self' data:` + `resourceDomains`；`font-src` 为 `'self'` + `resourceDomains`；
`connect-src` 为 `'self'` + `connectDomains`；`frame-src` 仅 `frameDomains`（缺省 `'none'`）；
`object-src 'none'`；`base-uri` 仅 `baseUriDomains`（缺省 `'self'`）。
`csp` 缺失时 MUST 使用上述严格缺省；宿主 MUST NOT 放行未声明的域名。

#### Scenario: 未声明域名被阻断
- **WHEN** 一个 app 只声明 `connectDomains: ["https://api.example.com"]`，却尝试
  `fetch("https://evil.example.net")`
- **THEN** 浏览器按 CSP 阻断请求；审计可见该 app 的 CSP 配置，宿主未放宽策略

#### Scenario: 无 CSP 元数据用严格缺省
- **WHEN** 资源的 `_meta.ui.csp` 缺失
- **THEN** 文档响应为严格缺省（`connect-src 'none'` 等），app 无任何外部连接

### Requirement: View 沙箱与跨端承载

web 客户端 View MUST 运行在不透明来源（opaque origin）：iframe `sandbox` MUST 含 `allow-scripts`，
MUST NOT 含 `allow-same-origin`、`allow-top-navigation`、`allow-popups`、`allow-modals`、`allow-forms`。
NSH（桌面）MUST 在不注入任何宿主能力的前提下承载 View。TUI MUST NOT 渲染 app（显示标题、
链接与文本兜底）。第二 origin 沙箱 MUST 作为可选增强（能力位 `mcp.apps.sandboxOrigin`）：
启用时用 sandbox 代理页 + 内层 View 的双 iframe，桥契约与单 origin 形态一致；
未启用时 MUST NOT 声称自己是不同 origin 沙箱，但 MUST 满足 opaque origin 与 CSP 等价约束。

#### Scenario: 沙箱属性断言
- **WHEN** 自动化 E2E 挂载一个 app
- **THEN** 断言 iframe 的 sandbox 恰为 `allow-scripts`（无 `allow-same-origin`）；违规即失败

#### Scenario: TUI 不渲染
- **WHEN** 会话在终端 idiom 下打开一个带 app 的工具结果
- **THEN** 不出现 WebView/HTML；显示 app 标题、文档链接与文本结果

#### Scenario: 第二 origin 可选
- **WHEN** 部署配置了第二 origin
- **THEN** 能力位为 true 且客户端走双 iframe 形态；未配置时能力位为 false，行为回到 opaque origin 形态

### Requirement: 握手与 hostContext

View 的 `ui/initialize` MUST 得到 `McpUiInitializeResult`（含 `protocolVersion`、`hostInfo`、
`hostCapabilities`、`hostContext`）。`hostContext` SHOULD 含 theme、styles.variables（宿主令牌映射）、
displayMode、availableDisplayModes、containerDimensions、locale、timeZone 与 platform。
宿主 MUST NOT 在收到 `ui/notifications/initialized` 之前向 View 发送任何 `ui/notifications/*`。

#### Scenario: initialized 前不发通知
- **WHEN** 一个 View 只发 `ui/initialize` 却未发 `initialized`（挂起/重连）
- **THEN** 宿主不发送任何 `ui/notifications/tool-*`；超时后实例销毁

#### Scenario: hostContext 可对表
- **WHEN** 抓取一次 `ui/initialize` 的响应
- **THEN** hostContext 的分节与 design §3.4 的映射表一致；未提供的分节不出现（不塞空值）

### Requirement: 数据流与生命周期通知

宿主 MUST 在握手完成后发送 `ui/notifications/tool-input`（**恰好一次**，参数为本次调用实参），
并在结果可用时发送 `ui/notifications/tool-result`（完整 `CallToolResult`：
content、structuredContent、isError）。工具被取消时 MUST 发送
`ui/notifications/tool-cancelled`。卸载前 MUST 发送 `ui/resource-teardown` 并等待响应
（有界，默认 1s）。弹性尺寸下 MUST 消费 `ui/notifications/size-changed` 调整尺寸；
主题/模式变化 SHOULD 发送 `ui/notifications/host-context-changed`（部分字段）。

#### Scenario: 时序正确
- **WHEN** View 完成握手
- **THEN** 宿主恰好发一次 `tool-input`，结果到达后恰好发一次 `tool-result`；顺序与次数被测试固化

#### Scenario: 工具被取消
- **WHEN** 运行被用户停止且该工具调用未产生结果
- **THEN** View 收到 `ui/notifications/tool-cancelled`，宿主随后销毁实例

#### Scenario: teardown 有界等待
- **WHEN** 组件卸载而 View 不回应 `resource-teardown`
- **THEN** 宿主在 1s 内销毁实例并继续（不阻塞界面）

### Requirement: 协议违规、超时与实例销毁

握手超时（默认 5s）、帧超限（默认 64 KiB）、超频或任何协议违规
MUST 导致该 View 实例被销毁并回落文本兜底 + 审计，
MUST NOT 留下半渲染的 iframe，MUST NOT 因单个 app 失败影响所在 run 的其它工具与会话。

#### Scenario: 超时销毁并兜底
- **WHEN** View 在 5s 内未完成握手
- **THEN** iframe 被销毁，工具结果区回到文本兜底；审计记录 `protocol` 超时

#### Scenario: 违规断连留痕
- **WHEN** View 发送未知方法或超过 64 KiB 的单帧
- **THEN** 请求被拒绝（未知方法回 `-32601`），实例按策略销毁，审计可见

### Requirement: tools/call 与 resources/read 由宿主代理

View 的所有出站动作 MUST 经宿主（postMessage 桥 → 宿主 HTTP 面），
MUST NOT 直连 MCP 服务器或 keel 内部 API。`tools/call` MUST 只允许该 app 所属服务器的、
`visibility` 含 `"app"` 的工具；响应 MUST 为标准 `CallToolResult`，
MUST NOT 透传服务器响应头。`resources/read` MUST 只允许该服务器，
URI MUST 过 scheme 白名单（仅 `ui://` 与 `http(s)://`；`file:`/`data:` 等一律拒绝）。

#### Scenario: app 调用自己的工具
- **WHEN** app 发 `tools/call {name:"refresh"}`
- **THEN** 宿主代理到该服务器并回填结果；审计记录 server/app/tool/结果

#### Scenario: 越权调用被拒
- **WHEN** app 请求调用另一个服务器的工具（或本服务器 `visibility` 不含 `"app"` 的工具）
- **THEN** 403 + 审计 `refused`；不产生任何上游请求

#### Scenario: 非法资源 URI
- **WHEN** app 请求 `resources/read {uri:"file:///etc/passwd"}`
- **THEN** 拒绝并审计；没有本地文件访问发生

### Requirement: ui/message 落点与确认

`ui/message` MUST 落到既有输入写路径（账本 `user.message`），
默认 MUST 经用户确认（decision 原语：发送/编辑后发送/丢弃）。
`role` MUST 只接受 `"user"`（其它值 MUST 回 JSON-RPC `-32602`）；
投递模式默认 `queue`，View MUST NOT 自选 `guide`；编辑分支 MUST 只预填 composer、不落账本。

#### Scenario: 消息需确认
- **WHEN** app 发 `ui/message {role:"user", content:[{type:"text",text:"…"}]}`
- **THEN** 出现确认（发送/编辑后发送/丢弃）；选「发送」后账本出现一条 `user.message`
  （requestId 可归属该 app 实例）；选「编辑后发送」只预填 composer

#### Scenario: 非法 role
- **WHEN** app 发 `ui/message {role:"assistant", …}`
- **THEN** 回 `-32602`，不产生任何账本写入

### Requirement: 链接、模型上下文与显示模式

`ui/open-link` MUST 只允许 `http`/`https`，默认 MUST 经用户确认，确认后 MUST 以
`noopener,noreferrer` 打开，MUST NOT 导航宿主页面。`ui/update-model-context` SHOULD 被记录为
会话的 app 上下文（覆盖式、下条用户消息并入）；实现未接线时 MUST 回 `-32601` 且
`hostCapabilities` 不声明该能力（MUST NOT 假装成功）。`ui/request-display-mode` MUST 只支持
`inline`/`fullscreen`、MUST NOT 切到 View 未声明的模式、MUST 返回**实际**生效模式。
未知的 `ui/*` 或核心方法 MUST 回 `-32601` 且无副作用；SDK 2.x 独有面
（`ui/download-file`、`ui/notifications/request-teardown`）MUST 明确拒绝而非静默吞掉。

#### Scenario: 非法链接被拒
- **WHEN** app 请求 `ui/open-link {url:"javascript:alert(1)"}`
- **THEN** 宿主拒绝并审计；宿主页面 URL 不变化

#### Scenario: 显示模式返回实际值
- **WHEN** app 声明只支持 `["inline"]` 却请求 `fullscreen`
- **THEN** 宿主回当前实际模式（`inline`），不切换；请求 `pip` 同样回实际模式

#### Scenario: 未接线能力不假装
- **WHEN** 本部署未接线 app 上下文
- **THEN** `ui/update-model-context` 回 `-32601`，`hostCapabilities` 无该项，能力位为 false

### Requirement: 安全、同意与审计

app 请求的浏览器能力（camera/microphone/geolocation/clipboardWrite）MUST 只在该 app 首次请求
且用户确认后授予（iframe `allow`）；用户拒绝后 MUST NOT 静默重试授予。
破坏性工具调用（`annotations.destructiveHint` 为真，或缺注解但名字命中变更族词表）
SHOULD 逐次确认。

宿主 MUST 为以下动作写持久审计：文档取回与校验失败、`tools/call`（含被拒）、`resources/read`、
`ui/message`（发送/编辑/丢弃）、`ui/open-link`（打开/拒绝）、权限授予与拒绝、协议违规。
审计行 MUST NOT 含消息正文、凭据、token 或完整 URL 查询串；
服务器凭据 MUST 只存在于宿主进程，MUST NOT 出现在 app 文档、桥消息、审计或日志中。

#### Scenario: 审计不含正文
- **WHEN** 扫描 app 审计的全部行
- **THEN** 无消息正文、无凭据、无 token；只有标识、动作、结果与有界摘要

#### Scenario: 权限拒绝不再静默重试
- **WHEN** 用户拒绝某 app 的麦克风权限，app 再次请求
- **THEN** 宿主仍不授予；审计记录第二次请求与拒绝

#### Scenario: 破坏性调用逐次确认
- **WHEN** app 调用一个声明 `destructiveHint` 的工具
- **THEN** 每次调用都有确认门；用户拒绝则不产生上游调用

### Requirement: 降级路径与能力协商

宿主 MUST 在 `initialize` 的 `ClientCapabilities` 中声明 MCP Apps 支持，使用扩展标识
`io.modelcontextprotocol/ui` 与 `mimeTypes:["text/html;profile=mcp-app"]`；
因早期 `extensions` 字段合法性变动，宿主 MUST 同时把同一 payload 放在
`capabilities.experimental` 下（两处内容一致），并在文档中记录该差异来源。

任何一步失败（取回失败、类型不符、握手超时、协议违规、CSP/权限拒绝、文档超限、命中拒绝哈希）
MUST 呈现可解释的降级：文本结果始终可用、错误原因可见、可重试；
MUST NOT 出现空白区、伪造 UI 或「假成功」。客户端无 `mcp.apps` 能力时 MUST 走同一降级。

#### Scenario: 取回失败可解释
- **WHEN** 文档取回超时或校验失败
- **THEN** 工具结果区显示文本结果 + 失败原因 + 重试入口；审计记录；会话其它部分不受影响

#### Scenario: 双位置能力声明
- **WHEN** 抓取宿主 `initialize` 请求
- **THEN** `capabilities.extensions["io.modelcontextprotocol/ui"]` 与
  `capabilities.experimental["io.modelcontextprotocol/ui"]` 都存在且内容一致

#### Scenario: 无 app 能力的老客户端
- **WHEN** 客户端不在能力表中声明 `mcp.apps`
- **THEN** 不请求 app 目录、不渲染 iframe；工具结果仍完整可读
