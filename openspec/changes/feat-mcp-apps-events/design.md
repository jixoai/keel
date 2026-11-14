# Design: feat-mcp-apps-events

> Owner 指令：MCP App 与 MCP events 两个标准都要对接。Keel 在 MCP 里是 **host/client**。
> 本文是**实现可直接照做**的设计：每一节都落到本仓文件与行号；标准口径先核实（§0），
> 现状对账（§1），注册面（§2），Apps 宿主（§3），events 接入（§4），测试与验收（§5），
> 分阶段拆解（§6），风险（§7）。附录 A 为标准出处清单。

---

## 0. 标准口径核实（先核实，再设计）

抓取日：**2026-11-14**。抓取方式：`cdn.jsdelivr.net/gh/<repo>@main/<path>`（raw.githubusercontent
在本机网络下不稳，jsdelivr 镜像内容与 GitHub main 同源），逐文件校对。**实现时必须按本节的确切
方法名/字段名，不得凭记忆拼写。**

### 0.1 MCP Apps（官方扩展，SEP-1865）

| 项 | 核实结果 | 出处 |
|---|---|---|
| 扩展标识 | `io.modelcontextprotocol/ui` | `ext-apps` `specification/2026-01-26/apps.mdx`（下称 apps.mdx），§Extension Identifier |
| 状态/日期 | **Stable (2026-01-26)**；Created 2025-11-21；同仓 `specification/draft/apps.mdx` 状态为 **Draft** | apps.mdx 头部；同上 draft 文件头部 |
| 能力声明位置 | apps.mdx 正文示例放在 `capabilities.extensions."io.modelcontextprotocol/ui" = {mimeTypes:[...]}`；`ext-apps` issue #231 记录该声明应从 `extensions` 移到 `capabilities.experimental`（旧版 `extensions` 不是 MCP 合法字段）；2026-07-28 核心修订才把 `extensions` 正式加进 `ClientCapabilities` | apps.mdx §Client↔Server Capability Negotiation；ext-apps issue 231；2026-07-28 changelog Minor #1 |
| UI 资源 | `ui://` scheme；`mimeType` MUST 为 `text/html;profile=mcp-app`；内容经 `resources/read` 返回（`text` 或 base64 `blob`） | apps.mdx §UI Resource Format |
| 资源 `_meta.ui` | `csp{connectDomains,resourceDomains,frameDomains,baseUriDomains}`、`permissions{camera,microphone,geolocation,clipboardWrite}`、`domain`、`prefersBorder` | apps.mdx §UI Resource Format；SDK schema `McpUiResourceMeta` |
| 工具 `_meta.ui` | `resourceUri`、`visibility:["model"\|"app"]`（缺省 `["model","app"]`；不含 `"model"` 的工具 **MUST NOT** 出现在 agent 工具面；`tools/call` 来自 app 时 MUST 校验 `"app"`）；废案 `_meta["ui/resourceUri"]` 平铺键需兼容读取 | apps.mdx §Resource Discovery / §Visibility |
| 握手 | View→Host：`ui/initialize`（params 必含 `appInfo`、`appCapabilities`、`protocolVersion`）→ Host 回 `McpUiInitializeResult{protocolVersion,hostInfo,hostCapabilities,hostContext}` → View 发 `ui/notifications/initialized`；**Host MUST NOT 在收到 initialized 之前发任何 `ui/notifications/*`** | apps.mdx §Lifecycle；SDK schema `McpUiInitializeRequest/Result` |
| 数据下行（Host→View） | `ui/notifications/tool-input`（必发一次，先于 tool-result）、`ui/notifications/tool-input-partial`（可选）、`ui/notifications/tool-result`（params 为完整 `CallToolResult`）、`ui/notifications/tool-cancelled`、`ui/notifications/host-context-changed`、`ui/notifications/size-changed`、`ui/resource-teardown`（Host MUST 拆除前发） | apps.mdx §Requests/Notifications（H→V） |
| 动作上行（View→Host） | `tools/call`、`resources/read`、`notifications/message`、`ping`（核心方法）；`ui/message{role:"user",content}`、`ui/open-link{url}`、`ui/request-display-mode{mode}`、`ui/update-model-context{content?,structuredContent?}` | apps.mdx §Standard MCP Messages / §MCP Apps Specific Messages |
| 显示模式 | `inline` \| `fullscreen` \| `pip`；View 在 `appCapabilities.availableDisplayModes` 声明；Host 在 `hostContext.availableDisplayModes` 声明；Host MUST NOT 切到 View 未声明的模式；`ui/request-display-mode` 的返回**实际**模式 | apps.mdx §Display Modes |
| 容器尺寸 | `hostContext.containerDimensions`，固定（`height`/`width`）与弹性（`maxHeight`/`maxWidth`）可组合；弹性时 Host MUST 监听 `ui/notifications/size-changed` 调 iframe 尺寸 | apps.mdx §Container Dimensions |
| 主题 | `hostContext.theme` + `styles.variables`（标准 CSS 变量名表）+ `styles.css.fonts` | apps.mdx §Theming |
| 沙箱（web host） | Host 与 Sandbox **MUST 不同 origin**；Sandbox `sandbox` MUST 含 `allow-scripts`、`allow-same-origin`；sandbox 先发 `ui/notifications/sandbox-proxy-ready`，Host 回 `ui/notifications/sandbox-resource-ready{html,csp?,permissions?}`；CSP 由资源元数据构造；无 `csp` 时用严格缺省（`default-src 'none'` 系） | apps.mdx §Sandbox proxy / §Host Behavior / §Security Implications |
| SDK | `@modelcontextprotocol/ext-apps`；GitHub main `package.json` version **2.1.0**，npm `latest` **2.0.4**；exports `.`/`./react`/`./app-bridge`/`./server`/`./schema.json`(+`-with-deps` 变体)；peerDeps 含 `@modelcontextprotocol/{client,server,core}@^2.0.0`、`zod@^4.2.0`、react 17–19 | ext-apps `package.json`；registry.npmjs.org |
| 与 stable 正文的 SDK 差异（需要容忍） | 2.x schema 里 `ui/message.params.content` 是**内容块数组**（stable 正文示例是单个 `{type:"text",text}` 对象）；`hostCapabilities` 多出 `downloadFile/updateModelContext/message/sampling`；新增 `ui/download-file`、`ui/notifications/request-teardown` | apps.mdx §ui/message；SDK generated schema |
| 公告 | 2026-01-26 发布，官方称「第一个官方扩展、ready for production」；四层安全叙事：iframe 沙箱、预声明模板、可审计消息、UI 发起工具调用需用户同意 | blog.modelcontextprotocol.io/posts/2026-01-26-mcp-apps/ |

**方法名全集（从 SDK generated schema 提取，17 个 `ui/*`）**：
`ui/initialize`、`ui/message`、`ui/open-link`、`ui/request-display-mode`、`ui/update-model-context`、
`ui/download-file`、`ui/resource-teardown`、
`ui/notifications/initialized`、`ui/notifications/tool-input`、`ui/notifications/tool-input-partial`、
`ui/notifications/tool-result`、`ui/notifications/tool-cancelled`、`ui/notifications/size-changed`、
`ui/notifications/host-context-changed`、`ui/notifications/request-teardown`、
`ui/notifications/sandbox-proxy-ready`、`ui/notifications/sandbox-resource-ready`。

### 0.2 MCP events（没有独立「Events 规范」；事件面在 Transports + 核心修订里）

| 项 | 核实结果 | 出处 |
|---|---|---|
| 官方是否有独立 Events 规范 | **没有**。事件面由 Transports（Streamable HTTP / stdio 的通知流）、`notifications/*` 方法表、以及 2026-07-28 的 `subscriptions/listen` 承载 | MCP spec 目录结构（`docs/specification/2026-07-28/**`）；2026-07-28 changelog |
| 2026-07-28 修订（Major） | ① 移除协议级 session 与 `Mcp-Session-Id`；② stateless：移除 `initialize`/`notifications/initialized`，每请求 `_meta` 携带 `io.modelcontextprotocol/protocolVersion`、`io.modelcontextprotocol/clientCapabilities`（`clientInfo`/`serverInfo` SHOULD）；③ 新增 `server/discover`（服务端 MUST 实现）；④ **`subscriptions/listen`** 取代 HTTP GET 端点与 `resources/subscribe|unsubscribe`；⑤ 移除 `ping`、`logging/setLevel`、`notifications/roots/list_changed`（日志级别改 `_meta` 的 `io.modelcontextprotocol/logLevel`）；⑥ tasks 移出核心成 `io.modelcontextprotocol/tasks` 扩展；⑦ **MRTR** 取代服务端主动请求；⑧ 所有 result 必带 `resultType`（`"complete"`/`"input_required"`；旧服务端缺字段客户端 MUST 当 `"complete"`）；⑨ 移除 SSE 可恢复性（`Last-Event-ID` 与 event id）与重投递，断流即丢请求、客户端 MUST 换新 id 重发 | 2026-07-28 `changelog.mdx`（Major 1–9，含 SEP 号 2567/2575/2663/2322） |
| 2026-07-28 修订（Minor，与实现相关） | `Client/ServerCapabilities` 加 `extensions`；Streamable HTTP POST 必带 `Mcp-Method`/`Mcp-Name` 头（`MCP-Protocol-Version` 与 body `_meta` 不一致 → 400 `HeaderMismatch`）；`tools/list`、`resources/list|read`、`prompts/list` 等结果必带 `ttlMs`/`cacheScope`（缓存提示）；资源未找到错误码 `-32002`→`-32602`；错误码分区（`-32022` UnsupportedProtocolVersion 等） | 同上 Minor 1/4/5/6/12 |
| `subscriptions/listen` 契约 | 请求 `params.notifications`：`toolsListChanged`/`promptsListChanged`/`resourcesListChanged`(bool)、`resourceSubscriptions`(string[])；服务端**首个**消息 MUST 是 `notifications/subscriptions/acknowledged`（带服务端实际接受子集），此前 MUST NOT 发任何通知；每条通知带 `_meta["io.modelcontextprotocol/subscriptionId"]`（值＝该 listen 请求的 JSON-RPC id）；stdout 上多订阅靠该字段去复用；取消＝关流（HTTP）或 `notifications/cancelled`（stdio）；服务端主动结束 SHOULD 先回 `resultType:"complete"` 响应再关流；stdio 断线重连后客户端 MUST 重发 listen（服务端不留订阅状态） | `docs/specification/2026-07-28/basic/patterns/subscriptions.mdx` |
| 通知分类（现行） | 请求域：`notifications/progress`、`notifications/message`（**只在所属请求的响应流上**，不上 listen 流）；目录域：`notifications/tools/list_changed`、`prompts/list_changed`、`resources/list_changed`；资源域：`notifications/resources/updated`（仅已订阅 URI） | subscriptions.mdx；streamable-http.mdx §Receiving |
| MRTR 契约 | 服务端对 `prompts/get`、`resources/read`、`tools/call` 可回 `InputRequiredResult{resultType:"input_required", inputRequests?, requestState?}`（二者至少一）；`inputRequests` 为 map<key, ElicitRequest\|CreateMessageRequest\|ListRootsRequest>；客户端 MUST 构造 `inputResponses` 并以**新的 JSON-RPC id** 重试原请求、MUST 原样回显 `requestState` 且 MUST NOT 解析/改写；服务端 MUST 把 `requestState` 当攻击者可控输入（完整性保护，建议含 principal/TTL/原请求摘要）；服务端 MUST NOT 发客户端未在能力里声明的请求类型；客户端 MUST NOT 在其它并行请求上复用 inputRequests/requestState | `docs/specification/2026-07-28/basic/patterns/mrtr.mdx` |
| 版本探测（向后兼容） | 客户端 MAY 先发「现代」请求探测：回 400 且 body 是**可识别的现代 JSON-RPC 错误** → 服务端讲现代版（按 advertised `supported` 重试或修正）；body 空或不可识别 → 回落 `initialize`（legacy 全程）；旧版服务端收到 GET/DELETE 应回 405，收到 `Mcp-Session-Id`/`Last-Event-ID` 应忽略 | streamable-http.mdx §Backward Compatibility / §Earlier Revisions |
| stdio 的现代形态 | 没有 session/HTTP 目标，同样每请求 `_meta`；`server/discover` 作为兼容性探测（changelog Major 3 原文） | changelog；mrtr/subscriptions 的 stdio 注记 |

**明确记录的口径纠正**（与任务书的差异，实现以官方为准）：
任务书说「2026-07-28 加入 `subscriptions/listen`、移除 session 与 Last-Event-ID 恢复、服务端→客户端
请求改为 MRTR」——与官方 changelog 完全一致；但官方同期还做了**删除 `initialize` 握手（stateless）
与新增 `server/discover`、`resultType` 必填**两件破坏性变更，设计必须一并覆盖，否则「支持
2026-07-28」是假的。

---

## 1. 现状对账（读码结论）

### 1.1 `internal/mcp` 现有能力（工具面，PI 行为移植）

| 能力 | 现状 | 位置 |
|---|---|---|
| 连接/握手 | `initialize` + `notifications/initialized`；版本白名单 4 项，最新 `2025-11-25` | `internal/mcp/client.go:142-220`；`protocol.go:24-34` |
| 客户端能力声明 | `ClientCapabilities{experimental,roots,sampling,elicitation}` 直通 initialize；**无 `extensions` 字段** | `protocol.go:335-340`；`client.go:49-51` |
| 服务端能力读取 | `ServerCapabilities{logging,prompts,resources(subscribe/listChanged),tools,completions}` 已解析并暴露 | `protocol.go:347-362`；`client.go:127-131` |
| 工具 | `tools/list`（全分页，1 000 页上限）、`tools/call`（timeout、ctx、Abort）；结果校验（content 数组、structuredContent 任意 JSON、isError） | `client.go:224-281`、`546-572` |
| 工具适配 | `mcp__<server>__<tool>` 命名（`CreateToolName`）；schema 归一；20 KB 输出裁剪；**不做 `_meta.ui` 提取、不做 `visibility` 过滤** | `tools.go:38-64`、`119-141`；`naming.go:31-48`；`output_limit.go:20-24` |
| 结果转模型内容 | text/image 直通、resource 解包、audio/资源链接占位符 | `content.go:58-105` |
| 传输 | stdio（进程 + stderr 转发 + 帧上限）与 Streamable HTTP（Accept 双类型、POST 逐消息、SSE 响应流、GET 流、`Last-Event-ID` 恢复、401/403 step-up、`Mcp-Session-Id`、`MCP-Protocol-Version`、405 视为无 GET 流） | `stdio_transport.go`；`streamable_http.go:309-435`、`568-587`、`608-681`、`689-778` |
| 多服务器管理 | 并发连接、部分失败上报、`Reconnect`、`Tools()`、`PromptSection()`、`Close()` | `manager.go:104-180`、`415-491` |
| 配置 | 共享 `mcpServers` 形状（command/args/env/cwd/url/headers/oauth/enabled/description/timeout）；global + project 合并；仅 override `enabled/exposure/toolExposure` | `config.go`；`configfile.go:31-140` |
| OAuth | discovery/DCR/CIMD/PKCE/loopback、PG 加密存储、刷新锁 | `internal/mcp/oauth/**`；`internal/store/mcp_oauth.go` |

### 1.2 缺失能力（缺口清单）

| # | 缺口 | 证据 |
|---|---|---|
| GAP-1 | 通知全部丢弃 | `client.go:387` 注释「Notifications are not consumed by this batch」；`receiveLoop` 只处理响应与服务端请求 |
| GAP-2 | 服务端请求一律 `method not found` | `client.go:408-422`（注释自认 roots/sampling/elicitation 是 future work） |
| GAP-3 | 无 `resources/*`、无 `prompts/*` 客户端方法 | `protocol.go:437-463` 只有结果类型；`client.go` 无对应方法 |
| GAP-4 | `_meta.ui` 未被读取，`visibility:["app"]` 未被过滤（工具面污染 + 标准违例） | `protocol.go:394-403`（`Tool.Meta` 在场）；`tools.go:44-62`（无过滤） |
| GAP-5 | 无 app 宿主：没有 `ui://` 取回、没有沙箱渲染、没有 postMessage 桥 | 全仓无 `ui://` / `resourceUri` / `ext-apps` 引用（除 `docs/RESIDUAL-PLAN.md:67-74` 的规划） |
| GAP-6 | 无 `subscriptions/listen`、无 `resultType`、无 MRTR | `streamable_http.go:750-760` 只发 GET 流；`protocol.go` 无 `InputRequiredResult` 类型 |
| GAP-7 | 注册面只在 boot 组装一次：MCP 工具/提示段进 host 后**没有**再发布路径（插件有） | `internal/servicehost/services.go:205-208` 在 `newPromptFactory` 里一次性读取；`host.go:665-699` 的 republish 只服务插件 sinks（`cmd/keel/serve.go:701-717` 接线） |
| GAP-8 | 工具结果里的 `StructuredContent` 不落账本（app 需要标准 `CallToolResult`） | `internal/agent/runner.go:681-702` 只搬 `Content`/`Details`/`IsError`；`internal/agentcore/message.go:130-144` 无字段；模型侧 `StructuredContent` 在场（`agentcore/tool.go:58-72`） |
| GAP-9 | 版本区间不含 `2026-07-28`；`2026-07-28` 的 session/GET 语义与现传输冲突 | `protocol.go:29-34` |

### 1.3 可能被误当「注册面」的既有面（边界澄清，避免实现走错门）

| 既有面 | 它是什么 | 与 MCP 的关系 |
|---|---|---|
| `internal/agentext` 插件 Runtime（`plugin.go:84-130`、`248-510`） | 声明式插件：ToolSpec/PromptSection/HookSpec/TriggerSpec/Effect；每次 `Apply` 全量 republish（`publish()` :491） | **本设计复用的语义来源**：MCP 以同样的 sink 形状注册，但 MCP 服务器不注册为 `agentext.Plugin`（§2.5 决策） |
| 插件 UI 托管 + `keel.plugin.*` 桥（`cmd/keel/plugins_ui.go`、`internal/agentapi/plugin_bridge.go:74-81`） | 插件自托管页 + 4 动词桥（state/call/approve/subscribe），同源、CSP 严格、enabled 门 | **不同物**：插件页是「我们自己的包」，MCP app 是「第三方服务器的 HTML」。可复用的只有纪律（CSP/安全头/缓存/审计），MCP app 的桥是 **JSON-RPC postMessage**，不是 HTTP 动词 |
| 产品 SSE（`internal/httpapi/server.go:174`、`651+`；`hub.go`） | 会话事件账本的 SSE 回放 + `Last-Event-ID` 续传 | **不同层**：它回放的是 keel 账本事实；MCP 的流是每连接、2026-07-28 不可恢复。设计禁止把两者混成一条（§4.3） |
| decisions（`internal/agentext/decisions.go:233-418`） | 审批/问句原语（open/answer/wait/list，durable + 幂等 + 事件化） | **MRTR 的落点**（§4.6）与 `ui/message` 的确认门（§3.6） |
| `internal/gateway` + `internal/externalgw` | 私有 DSH AgentBackend 协议（复用 MCP 式框架，但**故意不是 MCP**，`gateway/agent.go:1-30`、`protocol.go:82-86`） | 不在本 change 范围；文档更新时不要把它写成 MCP |
| 工具面 v2（`openspec/changes/keel-harness-v2/specs/tool-surface/spec.md`） | 目标形态：11 个内建聚合工具，`mcp` 工具提供 `list/describe/call`；`mcp__<server>__<tool>` 明确要**删除** | 设计的 app 绑定 **MUST NOT 依赖模型面工具名形态**（§2.4） |

### 1.4 目标 spec 版本差异表（keel 需要同时讲两代）

| 面 | ≤2025-11-25（现状/legacy） | 2026-07-28（modern） | keel 处置 |
|---|---|---|---|
| 握手 | `initialize` + `notifications/initialized` | 无握手；每请求 `_meta`；`server/discover` | era 门控（§4.7） |
| session | `Mcp-Session-Id` + DELETE | 无 | modern 下不发不存 |
| 服务端→客户端流 | HTTP GET 长流（可 `Last-Event-ID` 恢复） | 无 GET；`subscriptions/listen` 的 POST 响应流 | modern 下改 listen（§4.3） |
| 恢复 | `Last-Event-ID` + event id 重投递 | 移除；断流即丢请求，换新 id 重发 | modern 下禁止发送 Last-Event-ID |
| 服务端请求 | 主动 `sampling/createMessage` 等 | MRTR `InputRequiredResult` | 两类都处理（legacy 走既有 answerServerRequest 升级，modern 走 MRTR） |
| result | 无 `resultType` | 必带；缺省按 `"complete"`；未知值视为非法 | 容忍（§4.6） |
| 头 | `MCP-Protocol-Version`（客户端发） | 另加 `Mcp-Method`/`Mcp-Name`；与 `_meta` 不一致 → 400 | modern 下补齐（§4.7） |
| 缓存 | 无 | `ttlMs`/`cacheScope` | 解析并用于 app 资源缓存 TTL（§3.2） |
| 列表通知 | `notifications/{tools,prompts,resources}/list_changed` | 同通知名，但投递面改为 listen 流 | 同一 dispatcher（§4.1） |

---

## 2. 注册面设计（依赖注入口径）

**定位（Owner 2026-11-14 架构第一原则）**：插件即依赖，注册＝能力声明。MCP 在 keel 里是
一个**向宿主声明四类能力的注册者**。今天它有工具（走 `servicehost` 的 prompt 组装）与提示段，
本 change 补齐**资源、apps、events** 三类，全部用与 `agentext` 插件相同的
「**声明 → 每次装配全量发布 → 宿主 republish**」语义。

### 2.1 四类能力 × 现有注册面 对照表

| MCP 能力 | 今天的通道 | 现有同类接口（可对照） | 本 change 的接口 |
|---|---|---|---|
| 工具 | `mcpService.Tools()`（`servicehost/services.go:296`）→ `host.New` 的 ExtraTools（`services.go:205`） | `agentext.ToolSpec`（`plugin.go:33-41`）→ `Runtime.ToolSink`（`plugin.go:108`）→ `rt.RefreshProductTools`（`host.go:673`） | **复用现有**：MCP 工具走同一条 `[]agentcore.AgentTool` 发布；`_meta.ui.visibility` 在适配时过滤（§2.4） |
| 提示段 | `mcpService.PromptSection()`（`services.go:297`）→ `host.Section{Name:"mcp_servers"}`（`services.go:206-208`） | `agentext.PromptSection`（`plugin.go:58-64`）→ `PromptSink`（`plugin.go:115`）→ `rt.RefreshProductPrompt`（`host.go:691`） | **复用现有**；新段 `mcp_apps`（app 目录摘要，教会模型「这些工具带 UI」），与 `mcp_servers` 并列 |
| 资源 | 无 | 无（插件没有资源面；skill 是文件系统概念） | **新接口** `ResourceSpec{Server,URI,Name,MimeType,Size,Meta}` + `ResourceSink`；宿主只做**目录 + 按需 read**，不做成模型工具 |
| apps | 无 | 插件有 UI（`provides.ui` + `/plugins/<id>/`），但那是自托管包 | **新接口** `AppSpec{AppID,Server,Tool,ResourceURI,Visibility,CSP,Permissions,PrefersBorder,Hash,FetchedAt}` + `AppSink`；app 目录与文档托管见 §3.3 |
| events | 无 | `agentext.HookSpec` + 闭集 `HookEvent`（`hooks.go:18-41`，payload 只带标识） | **新接口** `EventRoute{Server,Interest,Delivery}` + `EventSink`：路由的是「MCP 通知 → 宿主动作」，不是可订阅的对外事件；对外可见的仍走既有 hook 事件（新增两个事件名，§4.4） |

### 2.2 新接口（Go 形状草案，落 `internal/mcphost`，避免 mcp 包反向依赖宿主）

```go
// internal/mcphost/surface.go（新包；mcp 包继续零宿主依赖）

// Surface is the complete declaration one MCP fleet publishes to the host.
// It is republished wholesale on every change (assembly semantics), never
// mutated in place.
type Surface struct {
    Generation int64
    Servers    []ServerState     // 连接状态/版本/能力/错误
    Tools      []agentcore.AgentTool
    Resource   []ResourceSpec
    Apps       []AppSpec
    Prompt     []Section
}

// Sinks are the host's registration points. Nil sink = the capability is not
// served by this deployment (fail closed, surfaced in /v1/capabilities).
// Tools/Prompt carry the MCP half only; the host-side aggregator merges it with
// the plugin half before republishing (cmd/keel/dynamic_surface.go).
type Sinks struct {
    Tools      func([]agentcore.AgentTool) error // MCP 那一半 → 宿主 aggregator
    Prompt     func([]Section) error             // MCP 那一半（含 mcp_servers/mcp_apps）
    Apps       func([]AppSpec) error             // 新：app 目录（agentapi 读）
    Resources  func([]ResourceSpec) error        // 新：资源目录
    Changed    func(ChangeEvent)                 // 新：注册面变更（审计 + hook 事件）
}
```

宿主侧的接线点（`cmd/keel/serve.go`）：
- **必须先有一个聚合器**：今天的 `newPluginRuntime`（`serve.go:701-717`）把**插件**的
  工具/提示段直接喂给 `rt.RefreshProductTools/RefreshProductPrompt`，而这两个方法接收的是
  「本进程的完整动态面」（`host.go:673-699` 的注释：组合 MCP + external + static + plugin）。
  MCP 若直接复用同一闭包，会把插件的面刷掉（反之亦然）。因此 `cmd/keel` 新增
  `dynamicSurface`（`cmd/keel/plugins_bridge.go` 邻位，新文件 `cmd/keel/dynamic_surface.go`）：
  持有 `{mcpTools, pluginTools, mcpSections, pluginSections}`，任一侧变更时**合并**后调用
  `rt.RefreshProductTools(union)` + `rt.RefreshProductPrompt(union)`；两侧的 sink 都只更新
  自己那一半再触发聚合发布。这条规则是 P2 的硬验收项（R2）；
- `Apps`/`Resources` 由 `mcphost.Registry` 持有，经 `agentapi.Config` 的新字段暴露给 HTTP 面
  （与 `PluginManager`/`Plugins` 同形状的注入，`agentapi/api.go:70-156`）。

**关键点：MCP 的工具/提示发布必须从 `newPromptFactory` 的一次性读取改为「经 sinks 发布」**，
否则 §4.4 的 `tools/list_changed` 热更新没有通路（GAP-7）。改动做法：`mcpService` 在
`Start()`（`services.go:303-308`）之后把 `manager.Tools()`/`PromptSection()` 交给 sinks
（首次发布），之后每次变更重发全量。

### 2.3 发布语义与 run 边界

- **全量 republish**：每次发布替换整个 MCP 面（工具/资源/apps/提示段），与
  `agentext.Runtime.publish()`（`plugin.go:491-510`）同规则；
- **generation**：`Surface.Generation` 单调递增，`/v1/capabilities` 与 `/v1/mcp/servers` 都携带，
  客户端据此判断「无需轮询全量」——与插件 inventory 的 `revision/generation`
  （`plugin_manager_view.go:105-135`）同构；
- **进行中的 run 不受影响**：`agent.Runner` 在每次 attempt 开头解析工具集
  （`runner.go:439-455`）并把 `tools.List()` 冻结进本次 attempt 的 `AgentContext`
  （`runner.go:617-620`）；`host.RefreshSections` 的契约也写明「已持有旧 registry 的 run 继续安全」
  （`host.go:92-99`）。所以热更新的语义是「**下一个 attempt 生效**」，不是「打断当前 attempt」；
- **prompt 同理**：`runner.systemPromptFor` 每次 attempt 从 host 读
  （`runner.go:120-134`），下一 attempt 看到新的 `mcp_servers`/`mcp_apps` 段。

### 2.4 工具元数据（`_meta.ui`）如何流向客户端契约

三种读者、三条路：

1. **模型面**：`visibility` 不含 `"model"` 的工具**不得**出现在模型工具列表。
   - v1 exposure（现状）：`AgentTools`（`tools.go:38`）跳过该类工具；
   - v2 exposure（`tool-surface` 的聚合 `mcp` 工具）：`mcp` 的 `list/describe` 对 app-only 工具
     也**不列给模型**（它们是 app 的 API，不是模型的）；
2. **UI 面（app 绑定）**：`mcp.AgentTool.Execute` 的结果 `Details` 中携带 app 绑定：
   `MCPToolDetails`（`tools.go:26-31`）新增两个字段
   `App *MCPAppBinding{AppID, ResourceURI, Visibility}` 与
   `StructuredContent json.RawMessage`（有界，默认 256 KiB，超限截断并打标）。
   理由：`Details` 已经随 `tool.result` 落账本（`runner.go:687-700`），
   所以绑定是**可回放事实**，UI 刷新/重连后仍能挂载 app；
   `StructuredContent` 不能进 `ToolResultMessage`（会进模型上下文，`agentcore/message.go:130-144`），
   只能走 `Details`。
3. **机器面**：`GET /v1/mcp/apps` 给出 app 目录（`AppSpec` 的 JSON 投影），供客户端在
   没有结果细节时（例如历史截断）也能解析；两份数据同源，MUST NOT 各自重算。

**MUST NOT** 把 app 绑定写进 `packages/chat-surface` 的冻结契约（`contract.ts:8-25` 再导出、
`types.ts:47-57` `ToolCallView` 无 `family`/额外字段），否则要动冻结契约；UI 从
`tool.result` 的 `details`（`web/src/data/wire.ts:39-48` 已透传 `details`）读取即可。

### 2.5 决策：MCP 不注册为 `agentext.Plugin`，而是复用其注册语义

| 方案 | 结论 |
|---|---|
| A. 注册成 `agentext.Plugin`（builtin `keel.mcp`，Tools/PromptSections/Effects） | **本 change 不采用**。理由：插件生命周期由插件 manager 的「安装/启用/停用」驱动（`plugin_manager.go`），而 MCP 连接的生命周期由 `mcp.json` 配置 + 网络/OAuth 驱动；把连接塞进 `Effects`（`plugin.go:68-71`）会让两条生命周期管一个连接，且 `PluginEnv`（`plugin.go:74-81`）不持有 store/网络能力。方向正确但属 `keel-harness-v2` 的 W12 收编范围 |
| B. 复用 sink 形状 + 宿主 republish（本设计采用） | MCP 作为「插件形状的注册者」：声明 `Surface`，只经 sinks 发布；未来收编为 `keel.mcp` 插件时，把 sinks 换成 `RuntimeOptions.Tools/Prompt` 的新增配对即可，**注册面契约不变** |

实现落点：`internal/mcphost` 不 import `cmd/keel`，sinks 由 `cmd/keel` 注入
（与 `productToolIndex`/`pluginToolBridge` 的注入位置相同，`cmd/keel/plugins_bridge.go:29-101`）。

---

## 3. MCP Apps 宿主设计

### 3.1 标识：appId、目录、哈希

- `appId = "mcpapp_" + hex(sha256(server + "\x00" + resourceUri))[:16]`（稳定、无路径注入、
  与 `naming.go` 的哈希后缀同风格）；appId 是 URL 与缓存键，**不是**服务器可控字符串；
- `AppSpec` 携带 `Server`、`Tool`（原名，不带宿主命名空间）、`ResourceURI`、`Title`、`MIMEType`、
  `Visibility`、`CSP`、`Permissions`、`PrefersBorder`、`Hash`（文档字节 sha256）、`FetchedAt`、
  `TTLMs`（来自 `resources/read` 结果的 `ttlMs`，modern 服务端才可能有）；
- 一个 `ui://` 资源可被多个工具引用 → 目录按 appId 去重，`Tools []ToolRef` 列出全部引用者。

### 3.2 取回与缓存

- 取回时机：连接后**按需**（工具首次进入模型工具面 + 首次被调用时）与**预取**（P3 起，
  `resources/list` 后对 `ui://` 且 `mimeType==text/html;profile=mcp-app` 的条目预热），
  预取受 `KEEL_MCP_APP_PREFETCH`（默认开）与并发上限（默认 4）约束；
- 取回用既有 `Client` 通道新增的 `ReadResource(ctx, uri)`（§3.3 的客户端方法），
  **不新开连接**；结果 MUST 校验 `mimeType` 为 `text/html;profile=mcp-app`（否则记诊断、
  不渲染，但保留文本兜底）；
- 缓存键 `(server, resourceUri, hash)`；`GET /v1/mcp/apps/{appId}/view` 以 hash 作缓存身份，
  客户端 `Cache-Control: public, max-age=31536000, immutable`（同 `plugins_ui.go:224-228` 的
  hashed asset 策略），文档内容变更即换 URL（`?v=<hash>` 或路径 hash）；
- 失效：`notifications/resources/updated`（若已订阅该 URI）与
  `notifications/resources/list_changed` 触发重取；`ttlMs` 未到不主动重取；
  重取失败保留旧版本（**不出现空白**），诊断写审计；
- 上限：单文档 ≤ `KEEL_MCP_APP_MAX_BYTES`（默认 2 MiB）；缓存总量有界（默认 64 MiB，
  LRU 淘汰）；超限记诊断、不渲染。

### 3.3 服务端（Go）改动清单

| 文件 | 改动 |
|---|---|
| `internal/mcp/client.go` | 新增 `ListResources`/`ListResourceTemplates`/`ReadResource`/`ListPrompts`/`GetPrompt`；`CallTool` 增 `CallOptions{ToolCallID string; ProgressToken any; InputResponses map[string]json.RawMessage; RequestState string}`（MRTR 重试与进度关联）；`receiveLoop` 的交由 dispatcher（§4.2） |
| `internal/mcp/protocol.go` | `SupportedProtocolVersions` 增 `"2026-07-28"`；新增 `InputRequiredResult`/`InputRequests`/`InputResponses`、`resultType` 常量、`ReadResourceResult` 的 `TTLMs`/`CacheScope`、`McpUiToolMeta`/`McpUiResourceMeta`（apps 元数据）解析结构；`ClientCapabilities` 增 `Extensions map[string]json.RawMessage` |
| `internal/mcp/tools.go` | `AgentTools` 增 `visibility` 过滤 + `_meta.ui` 提取；`MCPToolDetails` 增 `App`/`StructuredContent`；`Execute` 透传 `StructuredContent`（截断有界） |
| `internal/mcphost/surface.go`（新） | `Surface`/`Sinks`/`AppSpec`/`ResourceSpec`/`EventRoute`（§2.2） |
| `internal/mcphost/registry.go`（新） | app 目录 + 文档缓存 + 资源目录；`Snapshot()`（可并发读）+ `Generation()` |
| `internal/mcphost/apps.go`（新） | 取回/校验/CSP 构造（`BuildCSP(meta, doc)`）/主题映射（§3.4）/上限与 LRU |
| `internal/mcphost/events.go`（新） | 通知 dispatcher + listen 生命周期 + MRTR 执行器（§4） |
| `internal/mcp/streamable_http.go` | era 门控：modern 下不发 `Mcp-Session-Id`/`Last-Event-ID`、不依赖 GET 流、405 归为正常；`Send` 增 `Mcp-Method`/`Mcp-Name` 头与请求 `_meta` |
| `internal/mcp/manager.go` | 发布 `ServerState`（版本/能力/错误/重连次数）；`Connect` 后启动 dispatcher（有 sinks 时）；`Reconnect` 后重放订阅意图 |
| `internal/servicehost/services.go` | `mcpService` 增 sinks 与 `Publish()`；`Start()` 首次发布；`Stop()` 关闭 dispatcher 后再 `Close()` |
| `internal/servicehost/host.go` | 复用 `RefreshProductTools`/`RefreshProductPrompt`（无需新方法；`mcp_apps` 段按 `pluginSections` 形态追加） |
| `cmd/keel/serve.go` | 构造 `mcphost.Registry`，装配 sinks（复用 `newPluginRuntime` 的闭包形状），注入 `agentapi.Config`；`/v1/capabilities` 增能力位 |
| `internal/agentapi/mcp_apps.go`（新） | §3.3.1 的路由 |
| `internal/httpapi/server.go` | 路由挂载（`/v1/mcp/apps*` 与既有 `/v1/mcp/*` 并列，`server.go:186-187`） |
| `internal/store/mcp_app_audit.go`（新，P4） | 审计表 `mcp_app_audit(ts,tenant_id,session_id,server,app_id,action,result,detail)` + append/list（形状对齐插件审计，`store.PluginAuditEntry`） |

#### 3.3.1 HTTP 面（agentapi，全部要求租户边界 `X-Tenant-ID`，与 `tenantFrom` 同规）

| 路由 | 语义 |
|---|---|
| `GET /v1/mcp/servers` | 本进程的 MCP 面：`{generation, servers:[{name,transport,protocolVersion,era,connected,capabilities,errors,reconnectCount,tools,apps}]}`；变更身份＝generation |
| `GET /v1/mcp/apps` | app 目录（`AppSpec` 投影，含 `documentUrl`、`hash`、`csp` 摘要、`visibility`、`prefersBorder`） |
| `GET /v1/mcp/apps/{appId}/view` | app 文档本体（HTML），响应头：`Content-Type: text/html; charset=utf-8`、`X-MCP-App-Mime: text/html;profile=mcp-app`、**CSP**（§3.7）、`X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer`、hash 命名的 immutable 缓存 |
| `POST /v1/mcp/apps/{appId}/tools/call` | 宿主代理 `tools/call`：body `{sessionId, toolCallId?, requestId, name, arguments}`；**只允许**该 app（其服务器）声明了 `visibility` 含 `"app"` 的工具；返回标准 `CallToolResult`；写审计 |
| `POST /v1/mcp/apps/{appId}/resources/read` | 代理 `resources/read`（只允许该服务器，且 URI 经 §3.7 的 scheme 白名单） |
| `POST /v1/mcp/apps/{appId}/message` | `ui/message` 落点：默认开 decision 确认，见 §3.6；body `{sessionId, content}` |
| `POST /v1/mcp/apps/{appId}/context` | `ui/update-model-context`：P5 落盘（§3.6.3）；未接线时 501 且能力位为 false |
| `POST /v1/mcp/apps/{appId}/open-link` | `ui/open-link`：返回 `{opened:bool, reason?}`；默认需用户确认（返回 202 + decision id，或 `confirm:true` 直开） |
| `GET /v1/mcp/apps/audit` | 审计读（分页，与 `/v1/plugins/audit` 同形） |

**为什么 `tools/call` 必须由宿主代理**（而不是页面直连服务器）：① 服务器的凭据/会话在宿主侧，
页面不含凭据；② app 只被授权调用**自己服务器**的工具（跨服务器调用按 spec 一律阻断）；
③ 这是用户确认与审计的唯一卡口（spec §Security Implications 的「auditable communication +
user consent」）。**禁止**宿主把 `tools/call` 泛化成任意 MCP 工具调用（越权面）。

### 3.4 客户端渲染（web + NSH 同源一份 TSX）

- **挂载点**：工具结果卡。`web/src/screens/messages.tsx:394-515` 的 `ToolRow` 在结果可用时，
  若 `tool.result.details.app` 存在且 `capabilities["mcp.apps"]`，渲染
  `<McpAppView call={props.call} binding={...} sessionId={...}/>`；app 视图**替代**纯文本结果区，
  但 MUST 保留「参数/结果文本」的可展开兜底（文本是标准要求的降级，`content` 永远在）；
- **Surface 注册表不动**：`web/src/surfaces/registry.ts:22` 的 `SurfaceKind` 是「会话面」
  （按 provider kind 解析），MCP App 是「工具结果渲染器」，不属于会话面——
  不新增 `SurfaceKind`，避免把工具级 UI 抬成会话级语义；
- **内联实现（web）**：`web/src/surfaces/mcp-app/index.tsx` 渲染 `<iframe>`：
  - `src = documentUrl`（`/v1/mcp/apps/{appId}/view?v=<hash>`）；
  - `sandbox="allow-scripts"`——**MUST NOT** 含 `allow-same-origin`：View 拿到 opaque origin，
    读不到宿主 DOM/storage/cookie，且对宿主 origin 的 `fetch` 是无凭据跨源请求（直接失败），
    于是「所有能力必须走 postMessage 桥」是结构性的，不是纪律性的；
  - `allow`（Permission Policy）由 `_meta.ui.permissions` 与用户确认结果共同决定（§3.7）；
  - 尺寸：内联模式用弹性高度（`maxHeight` 由宿主给，默认 480px），监听
    `ui/notifications/size-changed` 调整 `height`；全屏模式用 kit 的 `Dialog`/`Sheet`
    （`web/src/ui/kit/components/dialog.tsx|sheet.tsx`，遵守「UI 组件禁手写、只用 kit」）；
  - `onLoad` 后开始握手（§3.5），`onUnmounted` 发 `ui/resource-teardown`；
- **NSH（TUI/桌面）**：复用 `web/src/screens/plugins.tsx:933-946` 的 `<WebView src onError>`
  先例，外加 `onMessage`（`types.d.ts:1182-1211`，页面侧 `window.nsh.postMessage(data)`）与
  `WebViewRef.postMessage`（`index.d.ts:1446`）做 host→view 通道；注入脚本（`injectedJavaScript`）
  仅做「把 `window.parent.postMessage` 桥到 `window.nsh.postMessage`」的 10 行适配，
  **不做任何宿主能力注入**；TUI（终端 idiom）**不渲染** app（沿用插件 UI 的 TUI 规则：
  显示标题 +「在浏览器打开」+ 文本兜底，`site/docs/zh/harness/plugins/ui.md` §4）；
  能力门：`web/nsh.json` 的 `webview.hosts` 需包含文档 URL 的 host（本机 127.0.0.1 已在 `*`）
  ——设计不扩宽 `*`，P3 起收紧为「`localhost`/127.0.0.1 + 配置显式列出的 app origin」；
- **主题映射**：`hostContext.styles.variables` 由 `web/theme.json` 的 `cssVars.{light,dark}` 映射到
  apps 标准变量名（表见下），`theme` 取当前明暗，`fonts` 不发（宿主不注入 @font-face）；
  View 未声明变量时按 spec 回落自己的缺省；
- **locale/timeZone/platform**：`platform.locale()`/`Intl.DateTimeFormat().resolvedOptions().timeZone`
  /`platform.idiom`（web/desktop/mobile）；`containerDimensions` 按显示模式给；
  `deviceCapabilities.touch` 由 `platform.idiom` 推。

**主题映射表（web/theme.json → apps 标准变量，逐项显式，缺项不发）**

| apps 变量 | Keel 令牌 |
|---|---|
| `--color-background-primary` | `card` |
| `--color-background-secondary` | `panel` |
| `--color-background-tertiary` | `muted` |
| `--color-text-primary` | `foreground` |
| `--color-text-secondary` | `muted-foreground` |
| `--color-border-primary` | `border` |
| `--color-border-secondary` | `header-border` |
| `--color-background-danger/success/warning` | `destructive`/`success`(若无则 `accent`)/`warning`(若无则 `accent`) |
| `--color-ring-primary` | `ring` |
| `--font-sans` / `--font-mono` | 同名令牌 |
| `--border-radius-sm/md/lg/full` | `radius-control`/`radius`/`radius-card`/`radius-pill` |
| `--shadow-hairline` | `glass-edge` 系（无对应则不发） |

### 3.5 postMessage JSON-RPC 桥契约（**这是跨端唯一的协议面**）

纯函数模块：`web/src/surfaces/mcp-app/bridge.ts`（无 DOM、可单测），
`view.tsx` 只做 DOM/iframe 生命周期，`index.tsx` 做与 store 的接线。

**帧形状**：JSON-RPC 2.0，`{jsonrpc:"2.0", id?, method, params?}` 与
`{jsonrpc:"2.0", id, result|error}`。所有帧过 `validateFrame()`：
必须是对象、`jsonrpc==="2.0"`、`method` 在**允许表**内、`params` 形状过 schema 子集校验、
单帧 ≤ 64 KiB、每 app 每秒 ≤ 200 帧（超限断连并审计）。**未知 method 一律回
`-32601`，不转交宿主**（默认拒绝）。

| 方向 | 方法 | 宿主行为 |
|---|---|---|
| V→H | `ui/initialize` | 校验 `appInfo.name/version`、`appCapabilities`；回 `McpUiInitializeResult{protocolVersion:"2026-01-26", hostInfo:{name:"keel",version}, hostCapabilities, hostContext}`；**在收到 `ui/notifications/initialized` 前不发任何 `ui/notifications/*`**；未 initialized 就发通知 → 协议错误，关闭该 app 实例 |
| V→H | `ui/notifications/initialized` | 之后补发 `tool-input`（从 store 的 assistant tool_call args 取，**恰好一次**）→ 若结果已在账本则立刻 `tool-result`；结果未到则等 `${toolCallId}` 的 `tool.result` 到达后再发（走 `store.subscribe`） |
| V→H | `tools/call` | 转发到 `POST /v1/mcp/apps/{appId}/tools/call`；`id` 原样回填；响应 `result` 为完整 `CallToolResult`；同时把这次调用**登记为 app 子调用**（审计 + 可选 UI 提示，不写账本，见 §4.1） |
| V→H | `resources/read` | 转发到 `POST .../resources/read`（只允许该服务器；URI 只允许 `ui://` 与 `http(s)://`，`file://`/`data:` 等一律拒绝，见 §3.7） |
| V→H | `notifications/message` | **单向丢弃到宿主日志**（结构化 `level`/`message`），不进聊天气泡、不进账本；`level` 白名单 debug/info/warning/error |
| V→H | `ping` | 直接回 `{}`（不转服务器） |
| V→H | `ui/message` | 见 §3.6.1（默认 decision 确认 → 走 `POST /v1/sessions/{id}/input` 的既有写路径） |
| V→H | `ui/update-model-context` | 见 §3.6.3 |
| V→H | `ui/open-link` | 见 §3.6.2 |
| V→H | `ui/request-display-mode` | 宿主支持集合 `["inline","fullscreen"]`；请求 `pip` → 返回当前模式；请求 `fullscreen`/`inline` → 切换并回实际模式；切到 View 未声明的模式 → 拒绝（回当前模式） |
| V→H | `ui/notifications/size-changed` | 弹性尺寸下调整 iframe（仅内联模式） |
| H→V | `ui/notifications/tool-input` / `tool-result` / `tool-cancelled` | 由 store 投影驱动（结果/取消来自 `tool.result` 与 `run.finished`/取消态）；`tool-cancelled` 在 run 被取消且该调用无结果时发 |
| H→V | `ui/notifications/host-context-changed` | 主题切换、显示模式变化、窗口尺寸变化时发**部分** `HostContext` |
| H→V | `ui/resource-teardown` | 卸载前发（等待 ≤ 1s 或首个响应），随后销毁 iframe/WebView |
| H→V（保留） | `ui/notifications/sandbox-proxy-ready` / `sandbox-resource-ready` | 仅 web 双 iframe 形态使用（§3.7 的 origin 配置）；单 origin 形态不发 |

**id 空间**：View 的请求 id 与宿主的通知不共享命名空间（通知无 id）；宿主对 View 的
`resource-teardown` 用独立 id 段（`"host-<n>"`），**MUST NOT** 与 View 的数值 id 冲突。

**不做的事（明确登记为 Non-Goal，避免被当成漏做）**：`tool-input-partial`（keel 无 provider 级
工具参数增量投影，收到也不发；View MUST 忽略）、`ui/download-file`（P6 之后单独立项）、
`pip` 模式、`sandbox-*` 双 iframe 之外的第三种宿主形态。

### 3.6 出站动作的落点

#### 3.6.1 `ui/message` → composer/队列

- 默认路径：宿主开一条 decision（`agentext.DecisionBroker.Open`，`decisions.go:233`，
  `kind:"mcp-app.message"`，options `[{id:"send",label:"发送"},{id:"edit",label:"编辑后发送"},{id:"discard"}]`），
  **决定卡复用现有审批 UI**；`send` → 调既有输入写路径 `POST /v1/sessions/{id}/input`
  （`internal/httpapi/server.go:291-384` → `commitUserInput`，`input_commit.go:58`），
  `requestId = "mcp-app:" + appId + ":" + <View 的帧序号>`（幂等且可归属）；
  `delivery` 默认 `queue`（不打断运行中的 turn），View 不能自选 `guide`；
  `edit` → 走 `prefillComposer`（`web/src/screens/composer.tsx:55-58`）**不落账本**，由用户决定发送；
- 免确认：仅当用户在该会话勾选「此 app 可自动发消息」（存在 session 级偏好里，P4 加；
  默认关）；
- **MUST NOT** 由 app 直接注入 assistant/tool 角色内容；`role` 只接受 `"user"`，
  其它值回 `-32602`。

#### 3.6.2 `ui/open-link`

- 白名单：`http`/`https`；其余 scheme（`javascript:`/`data:`/`file:`/`vscode:`…）一律拒绝并审计；
- 默认需确认（与对外链接纪律一致：给用户看域名）；确认后 web 用 `window.open(url,"_blank",
  "noopener,noreferrer")`，NSH 用 `openUrl`（`web/nsh.json` 已声明 schemes `https`/`http`）；
- **MUST NOT** 用宿主页 `location.assign`；MUST NOT 传递宿主 URL 参数（不泄露 sessionId）。

#### 3.6.3 `ui/update-model-context`

- 语义：**覆盖式**的、下个 user 消息之前有效的 app 上下文（spec 原文：每次覆盖、可在下条
  用户消息时并入）；
- keel 落法（P5，最重的一项）：DB 表 `mcp_app_contexts(tenant_id, session_id, app_id, content jsonb,
  structured jsonb, updated_at)`（`internal/store` 新文件 + 迁移）＋
  `agent.Runner` 新 seam `SessionContextSource func(ctx, scope) []agentcore.Content`（`runner.go` 装配
  `agentCtx` 处注入，`runner.go:610-620` 附近），由 `internal/servicehost` 实现并把内容作为
  `<mcp_app_context>` 段附加到本次 attempt 的**最后一条 user 消息前**（与 `presentUnreadInputLast`
  的既有做法一致，`runner.go:470`）；
- 无 seam（未接线/迁移未跑）→ 能力位 `mcp.apps.context` 为 false、`hostCapabilities.updateModelContext`
  不发、请求回 `-32601`（spec 允许：这是 SHOULD）；
- **MUST NOT** 写进事件账本（不是会话事实、不进 R1 规则）：显式拒绝在 kernel 加 kind 的方案，
  理由记录在 §7-R5。

### 3.7 安全（全部写成 requirement）

1. **沙箱**：web 内联与全屏 iframe 都是 `sandbox="allow-scripts"`（**无 `allow-same-origin`**、
   无 `allow-top-navigation`、无 `allow-popups`、无 `allow-modals`、无 `allow-forms`）；
   NSH WebView 不接受宿主注入能力（注入脚本只做 postMessage 转发）；
2. **CSP**（`/v1/mcp/apps/{appId}/view` 响应头，由资源 `_meta.ui.csp` 构造，逐项对照 spec）：
   `default-src 'none'`；`script-src 'self' 'unsafe-inline' <resourceDomains>`；
   `style-src 'self' 'unsafe-inline' <resourceDomains>`；`img-src 'self' data: <resourceDomains>`；
   `font-src 'self' <resourceDomains>`；`media-src 'self' data: <resourceDomains>`；
   `connect-src 'self' <connectDomains>`；`frame-src <frameDomains|'none'>`；
   `object-src 'none'`；`base-uri <baseUriDomains|'self'>`；缺 `csp` 时用 spec 的严格缺省；
   **只收紧不放宽**（MUST NOT 出现未声明的域名）；
   另加 `frame-ancestors 'self'`（宿主只允许自己嵌）；
3. **permissions**：`camera`/`microphone`/`geolocation`/`clipboardWrite` 只在该 app 首次请求
   且用户确认后写入 `allow` 属性；确认结果按 `(server, appId, permission)` 记偏好；拒绝后
   **MUST NOT** 静默重试给权限；
4. **确认门**：`tools/call` 中**破坏性工具**（`annotations.destructiveHint===true` 或服务器未声明
   注解且工具名命中变更族，`web/src/screens/tool-families.ts:23-36` 的词表复用）默认逐次确认；
   `ui/message`、`ui/open-link` 默认确认（§3.6）；
5. **审计**（`mcp_app_audit`）：app 文档取回/校验失败、`tools/call`（谁、哪个 app、哪个工具、
   结果 ok/error/refused）、`resources/read`、`ui/message`（send/edit/discard）、`open-link`
   （opened/denied + host）、权限授予/拒绝、协议违规（未知方法/超频/超 size）；
   读取口径与 `/v1/plugins/audit` 同形；审计行**MUST NOT** 包含消息正文或凭据（只存标识、
   动作、结果与目标的截断摘要）；
6. **凭据隔离**：服务器 headers/OAuth token 只存在于宿主（`internal/mcp/oauth/**`），
   app 文档与桥永不携带；`tools/call` 响应**MUST**按 `ContentBlock` 透传但**MUST NOT**透传
   服务器响应头；
7. **供应链**：文档 hash 进目录与 URL；宿主可选 `KEEL_MCP_APP_DENY_HASHES` 黑名单，
   命中即不渲染（审计为 `refused`）；
8. **资源上限**：文档 2 MiB、缓存 64 MiB（LRU）、单 app 并发请求 4、每秒 200 帧、
   `structuredContent` 透传 256 KiB——全部有界且超限有诊断，不静默截断 JSON 结构
   （超限时 `tool-result` 仍发，但 `_meta.truncated=true`）；
**deviation 登记（诚实记录）**：spec 要求 web host 的 Sandbox 与 Host **不同 origin**。
keel 是单 origin 部署（`keel serve` 同源托管 web 与 API，`cmd/keel/serve.go:360-380`），
无法凭空造出第二 origin。本设计的等价约束是：View **opaque origin**（无 `allow-same-origin`）+
响应头 CSP + `frame-ancestors 'self'` + 无凭据跨源 fetch 必然失败 + 桥白名单；
并把「第二 origin」做成**可选增强**：`KEEL_MCP_APP_ORIGIN`（如
`mcpapp.<host>` / 另一端口）配上时，客户端走
「同源沙箱页 `/v1/mcp/apps/sandbox.html`（`sandbox="allow-scripts allow-same-origin"`）→ 内层
iframe 指向该 origin 的 `/view`」的双 iframe 形态（`/v1/capabilities` 的
`mcp.apps.originSandbox` 报 true/false）。两种形态下 §3.5 的桥契约一致。

### 3.8 降级路径（MUST 写成 requirement）

| 场景 | 行为 |
|---|---|
| 服务端不支持 apps（无 `_meta.ui`） | 纯文本结果，零变化（现状） |
| 客户端不支持 apps（capability false / TUI） | 纯文本结果 + 「此结果带交互界面」提示 + 文档链接（不自动打开） |
| 文档取回失败/校验失败 | 文本兜底 + 诊断（审计）+「重试」按钮；**不出现假 UI** |
| View 握手超时（默认 5s）或协议违规 | 销毁 iframe，回文本兜底，审计 `protocol` |
| CSP/permission 被用户拒绝 | View 仍渲染（缺能力时应按 spec 做特性检测），宿主记录拒绝原因 |
| App 请求了服务器未声明的工具 | `403` + 审计 `refused`（对齐插件桥的 ownership 复核纪律，`cmd/keel/plugins_bridge.go:68-101`） |

---

## 4. MCP events 接入设计

### 4.1 通知分类与落点（「哪些进账本」的裁决表）

| 通知 | 语义 | 落点 | 落账本？ | 可回放？ |
|---|---|---|---|---|
| `notifications/progress` | 某请求进度（token 由我们发出去） | 关联到 `(session, toolCallId)`（P4 起 `CallOptions.ProgressToken` 由 runner 设置）→ 转成**瞬态 UI 进度**（复用 `PartialHub` 的「建议性、可丢」语义，`agentext/turns.go:349-367`）→ 也可入遥测指标 | **否**（建议性） | 否 |
| `notifications/message` | 服务器日志 | 结构化日志（`level`/`logger`/`data` 截断 256B）+ 可选指标；`notifications/message` **MUST NOT** 出现在聊天时间线 | 否 | 否 |
| `notifications/tools/list_changed` | 工具目录变了 | 注册面热更新（§4.4）+ `ChangeEvent` + hook 事件 `mcp.surface.changed`（审计） | 否 | 否 |
| `notifications/prompts/list_changed` | prompts 目录变了 | 刷新 prompts 目录（P4 的资源面） | 否 | 否 |
| `notifications/resources/list_changed` | 资源目录变了 | 刷新资源目录；app 预取队列按新目录增量更新 | 否 | 否 |
| `notifications/resources/updated{uri}` | 已订阅 URI 内容变了 | 失效缓存 + 重取（app 文档立即换 hash）+ 审计 | 否 | 否 |
| `notifications/cancelled{requestId}` | 服务器取消我们的请求 | 中止该调用（否则等 timeout）；转成 `tool.result`（错误）——**这条最终会落账本**，因为它是这次工具调用的结果 | 由结果路径落 | 是（经 tool.result） |
| `notifications/subscriptions/acknowledged` | listen 被接受（含实际子集） | 更新订阅状态；请求了但被拒的类别记诊断（不静默） | 否 | 否 |
| 服务端 `Request`（legacy） | roots/sampling/elicitation（旧协议主动请求） | 走 §4.6 的同一执行器（能力探测后决定答或拒） | 由结果路径 | 是（若转成 decision/sampling 结果） |
| `InputRequiredResult`（modern） | MRTR 中间结果 | §4.6 | 由结果路径 | 是 |

**裁决依据（写进 spec）**：账本 = 会话事实（`internal/kernel/kernel.go:74-96` 的封闭 kind 表）。
服务器目录变化、日志、进度、订阅 ack 都**不是**会话事实，也**没有会话作用域**；
把它们写进某个会话的账本会污染转录与 R1 一致性规则。真正需要「可回放事实」的只有
**工具调用结果**（已有 `tool.result` 通路）与被 MRTR 转成 decision 的问句（已有 decision 通路）。

### 4.2 传递路径（client → dispatcher → sinks）

```
transport.Receive ──► Client.receiveLoop ──► Dispatcher
   (Response 分流)         │                    ├─ Progress/Message   → 瞬态通道（PartialHub/日志）
                           │                    ├─ list_changed/updated → Registry 刷新 → Sinks.Apps/Resources/Tools/Prompt
                           │                    ├─ Request(legacy) / InputRequiredResult(modern) → MRTR 执行器
                           └─ pending call 响应（既有）
```

`Dispatcher` 接口（`internal/mcphost/events.go`）：

```go
type Dispatcher interface {
    Notification(ctx context.Context, server string, method string, params json.RawMessage)
    ServerRequest(ctx context.Context, server string, req *mcp.Request) (mcp.Message, error) // legacy era
    InputRequired(ctx context.Context, server string, call CallRef, result mcp.InputRequiredResult) (mcp.CallToolResult, error)
}
```

`Client`（`internal/mcp`）只多一个 `opts.Notifications`/`opts.ServerRequests` 回调（形状见
`ClientOptions`，`client.go:45-57`），**不 import 宿主包**；`receiveLoop`（`client.go:368-390`）
把 `*Notification` 与新解析出的 `InputRequiredResult` 交给回调。这与现有
`answerServerRequest`（client.go:411）是替换关系（默认实现保持「method not found」，
业务实现由 host 注入——fail closed 不变）。

### 4.3 `subscriptions/listen` 生命周期

- **谁订阅**：`mcphost` 的**每服务器一个**订阅者（一个 `mcp.Client` 一条 listen 请求），
  **不是**每会话/每 UI 一处：MCP 目录是进程级事实，多会话共享（`Manager` 已是每进程一个，
  `manager.go:93-100`）；
- **filter（订阅意图）**：`toolsListChanged:true`、`promptsListChanged:true`、
  `resourcesListChanged:true`；`resourceSubscriptions` 默认**空**（只在我们真的有 app 且需要
  实时失效时加入具体 `ui://` URI——由 Registry 决定，避免为无关资源订阅）；
- **ack**：首个消息必须是 `notifications/subscriptions/acknowledged`；ack 的子集成为
  「实际订阅」事实，缺项写诊断（例如旧服务端不支持 listen 时我们走 GET 流，ack 永不到达，
  超时后回退，见下）；
- **断线重连**：
  - modern：断流即重发 `subscriptions/listen`（**MUST**，服务端无状态；`Last-Event-ID` **禁止**发送）；
  - legacy：GET 流中断按既有 `runGetStream` 退避重连（`streamable_http.go:702-747`），
    带 `Last-Event-ID`（若服务端给过 id）；
  - 重连成功后**MUST**全量重算一次目录（因为断线期间的变化没有回放）——这是与 keel 产品 SSE
    最大的区别；
- **与 keel SSE 的区别（写进 requirement，避免实现混淆）**：

  | | keel 产品 SSE（`/v1/sessions/{id}/events/stream`） | MCP 通知流（listen/GET） |
  |---|---|---|
  | 真值源 | 账本（durable，可 keyset 重读） | 服务器内存/状态（无账本） |
  | 恢复 | `Last-Event-ID` 续传（`hub.go:110` `parseAfter`，断言不漏事件） | modern 无恢复；legacy 有 event id 但无重投递保证 |
  | 作用域 | 会话 | 服务器（进程级） |
  | 消费者 | 浏览器/TUI | 宿主自身 |

  实现上两者**MUST NOT** 共用一个连接池或一个 handler：前者是 `internal/httpapi`，后者在
  `internal/mcphost`。

### 4.4 `tools/list_changed` → 注册面热更新

1. dispatcher 收通知 → `Registry.MarkDirty(server, "tools")`；
2. 去抖（默认 250ms 合并连发）后重算：
   - `client.ListTools()`（既有，`client.go:224`）→ 过滤/提取（§2.4）→ 与其它服务器的工具合并；
   - 重算 `mcp_servers` / `mcp_apps` 提示段；
3. 经 sinks 全量 republish：`Runtime.RefreshProductTools` + `RefreshProductPrompt`
   （`host.go:673/691`）——**复用插件通路**（`cmd/keel/serve.go:701-717` 的闭包），
   所以工具索引（`productToolIndex.publish`）与模型注册表同步更新；
4. `Generation++`；`ChangeEvent{Server,Kind,Added,Removed,Generation}` 写审计 + 触发 hook 事件
   `mcp.surface.changed`（新增到闭集，`hooks.go:18-41` 只能追加，符合「集合只增」注释）；
5. **失败语义**：重算失败（服务器断连/列表错误）→ **保留上一版工具面**（绝不发布空面），
   诊断 + 审计；`/v1/mcp/servers` 报该服务器的错误；
6. **run 边界**：进行中 attempt 用它开头冻结的 registry（§2.3）；`mcp.surface.changed`
   对运行中的 run 可见（hook 事件），但**不打断**它；
7. 测试要求：fake server 在 run 中途改工具列表 → 断言当前 attempt 的 `agentCtx.Tools` 不变、
   下一次 attempt 变化、`generation` 单调、旧工具名在下一 attempt 返回 unknown_tool。

### 4.5 通知 → 瞬态 UI 的通道（P4，可独立降级）

- `progress`：`CallOptions.ProgressToken = toolCallId`（字符串 token，最简且能反查）；
  dispatcher 按 `(server, token)` 找到 `(tenant, session, toolCallId)`（runner 在 `Execute`
  前注册映射，`tools.go:119-141`），发布到 `PartialHub` 的一次性 kind
  `mcp.progress`（payload 只有 `progress/total/message`，**绑定 toolCallId**）；
  `partials` SSE 端点（`internal/httpapi/partials.go`）已按会话转发，UI 在工具行显示进度条；
  丢帧无害（建议性）；
- 若要显示在工具行而不是聊天流：`Partial` 的 `MessageID` 字段复用作 `toolCallId`
  （`turns.go:349-355`），web 侧在 `ToolRow` 里消费；这条路径**不**要求账本变化。

### 4.6 MRTR 与既有 decision/consent 的对接

**流程**：`CallTool(ctx, name, args, opts)` 收到 `resultType:"input_required"` 时：

1. 解析 `inputRequests`（map key → request object）与 `requestState`（原样字节保存）；
2. 逐条执行请求：
   | 请求 | keel 行为 |
   |---|---|
   | `elicitation/create`（form 或 url 模式） | `DecisionBroker.Open`（`decisions.go:233`）开一条 `kind:"mcp.elicitation"` 的决定：`message` 进 prompt、`requestedSchema` 的字段映射为 options（枚举→选项；自由文本→`allowFreeText`）；URL 模式把 URL 放在选项描述里（**不**自动打开）；用户答案 → `ElicitResult{action:"accept"|"decline"|"cancel", content:{...}}` |
   | `sampling/createMessage` | 用「当前会话的模型服务」执行（`ModelService`/runner 的 provider seam）；**受配额与开关**：`KEEL_MCP_SAMPLING=off|session|on`（默认 `session`＝仅在 app/工具所属会话运行中允许）；模型不可用时回 `CreateMessageResult` 的等价错误（拒绝而非伪造）；`maxTokens` 上限（默认 4096）与请求数预算（默认每工具调用 3 次） |
   | `roots/list` | **拒绝**：keel 无 roots 处理器（与现代 spec 的 Deprecated 一致），回 JSON-RPC 错误 → 服务器应优雅失败；审计 |
   | 未知 method | 拒绝（`-32601`），**MUST NOT** 静默省略（省略会让服务器误以为客户端拒绝协作） |
3. 组装 `inputResponses`（key 与 `inputRequests` 一一对应）→ **以新 JSON-RPC id 重试原请求**，
   带上 `opts.InputResponses` 与 `requestState` 原样回显（`requestState` 只透传，**MUST NOT** 解析/改写）；
4. **轮次预算**：同一工具调用的 MRTR 往返 ≤ `KEEL_MCP_MRTR_MAX_ROUNDS`（默认 5）；
   超出 → 以 `tool.result` 失败（错误消息含「服务器要求多轮输入超过预算」），审计；
5. **超时**：每一轮的 wait 受调用超时约束（既有 `RequestTimeout`，`client.go:24`）；
   用户不答 → `ElicitResult{action:"cancel"}`（decision 超时路径已存在，`decisions.go:374`）；
6. **类型容忍**：`resultType` 缺失 → 视为 `"complete"`（spec MUST）；出现未知值 → 协议错误
   （spec：未识别值 MUST 视为 invalid），以 `tool.result` 失败并审计；
7. **legacy era 的等价物**：服务端主动 `Request`（roots/sampling/elicitation）走同一执行器：
   `ClientOptions.ServerRequests` 回调把 request 交给 dispatcher，把返回组装成
   `SuccessResponse`/`ErrorResponse`（替换 `client.go:411-422` 的 method-not-found 默认）；
   **能力声明**：只有真正实现了 elicitation/sampling 才在 `ClientCapabilities` 里声明
   （spec：MUST NOT 发客户端未声明类型的请求；反向也成立：我们要么能答、要么声明不支持）。

### 4.7 版本 era 与向后兼容

- `LatestProtocolVersion` **保持不变**（`2025-11-25`，PI 基线，`protocol.go:24`）；
  `SupportedProtocolVersions` 增 `"2026-07-28"`（`protocol.go:29-34`）；
- `ClientOptions.Era ProtocolEra`（`"auto"|"legacy"|"modern"`，默认由
  `KEEL_MCP_PROTOCOL_ERA` 决定，缺省 `"auto"`）：
  - `auto` 且 transport 为 HTTP：按 spec 的兼容探测算法——先 POST 一个带 `_meta`
    与 `Mcp-Method` 头的现代请求（`server/discover`）；400 且 body 是识别的现代错误
    （`UnsupportedProtocolVersion` 等）→ 现代 era，按 `supported` 重选版本重试；
    400 但 body 空/不可识别 → 回落 `initialize`（legacy）；
  - `auto` 且 transport 为 stdio：先发 `server/discover`，方法不存在/错误 → legacy；
  - legacy：现状路径完全不变（PI 一致性套件覆盖）；
- modern 下的传输行为（`streamable_http.go` 门控）：
  - 不发 `Mcp-Session-Id`（也不接受服务端发的）、不发 `Last-Event-ID`、不 DELETE session；
  - POST 必带 `MCP-Protocol-Version`（与 `_meta` 一致）、`Mcp-Method`、以及
    `tools/call`/`resources/read`/`prompts/get` 的 `Mcp-Name`（非 ASCII 用 spec 的 base64 哨兵编码）；
  - 每个请求 body 带 `_meta{io.modelcontextprotocol/protocolVersion, clientCapabilities,
    clientInfo}`（`RequestMeta()` 统一注入，避免逐处手写）；
  - 收到 405（GET/DELETE）视为正常；不期待 GET 流；
  - `ttlMs`/`cacheScope`：解析并用于缓存（`resources/read` 的 app 文档，
    `cacheScope:"public"` 才允许跨租户共享缓存，默认按 private 处理）；
- era 与能力的可观测性：`/v1/mcp/servers` 每服务器报 `era`、`protocolVersion`、
  `capabilities`（含 `experimental/extensions`），供 UI 与运维判断「对端讲哪一代」；
- **PI 一致性红线**：`scripts/conformance.sh`、`internal/mcp/*_test.go`（PI 移植测试）
  必须全绿；新增行为只走新代码路径（era 门控），不改 legacy 分支语义。

---

## 5. 测试与验收计划（逐条对 G0–G5）

口径：`docs/MATURITY.md`（G0 规格 → G1 单机 → G2 崩溃一致 → G3 可观测 → G4 PI 对齐 → G5 规模）。

### 5.1 夹具：fake MCP server（两种传输）

- **stdio**：扩充既有 `TestHelperProcess` 模式（`internal/mcp/stdio_transport_test.go:23-60`）：
  新增 `KEEL_MCP_HELPER_SCENARIO=apps-events`，行为集：
  - `initialize` 协商（按 env 选 `2025-11-25` / `2026-07-28`）；
  - `tools/list`：含 3 个工具——`show_dashboard`（`_meta.ui.resourceUri=ui://scenario/dashboard`）、
    `refresh`（`visibility:["app"]`）、`plain`；
  - `resources/list` + `resources/read`（返回带 `csp.connectDomains` 的 HTML）；
  - `tools/call`：`show_dashboard` 正常返回；`refresh` 判 `visibility`；
  - 通知：连接后可按脚本发 `notifications/tools/list_changed`、`resources/updated`、`progress`；
  - `subscriptions/listen`：ack + 按 filter 投递（modern）；GET 流（legacy）；
  - MRTR：`tools/call` 首次回 `InputRequiredResult{inputRequests:{login:elicit...}}`，
    重试（带 `inputResponses`+`requestState`）才成功，并断言 `requestState` 原样回显、
    重试 id 不同；
  - 主动请求：legacy 下 `sampling/createMessage`。
- **streamable HTTP**：`httptest.NewServer`（参考 `streamable_http_test.go:150-300`）同场景，
  两个 era 分支；modern 分支断言：无 `Mcp-Session-Id` 响应头、拒绝带 session 的请求、
  `Mcp-Method`/`Mcp-Name` 校验（错配回 400 `HeaderMismatch`）、`subscriptions/listen` 的
  响应流只带 opt-in 的通知、请求域通知只出现在对应请求的响应流。

### 5.2 分层测试矩阵

| 层 | 测试 | 位置 | 验收口径 |
|---|---|---|---|
| G0 | 两份 spec 与 `docs/PI-PARITY.md` 偏离登记 | 本 change | 方法名/字段/schema 与 §0 逐一可对表；`openspec validate --strict` 通过 |
| G1 | `internal/mcp` 单测：era 探测、`resultType` 容忍、MRTR 轮次、通知分发、`visibility` 过滤、`_meta.ui` 提取、资源读取、listen ack/断线重发 | `internal/mcp/*_test.go` | 无网络、无真模型可跑完（fake server 两条传输） |
| G1 | `internal/mcphost` 单测：CSP 构造（含严格缺省/只收紧）、主题映射、上限/LRU、目录与 hash、dispatcher 去抖与失败保留旧面、预算与审计行 | `internal/mcphost/*_test.go` | 表驱动；越权/超频/未知方法全部回拒绝且写审计 |
| G1 | 桥契约纯函数测：`web/src/surfaces/mcp-app/bridge.test.ts`（nsh test） | 帧校验、允许表、id 空间、握手时序（initialized 前不发通知） | `nsh test` |
| G1 | 端到端（单机）：`keel serve` + fake server（stdio）+ 脚本会话 | `cmd/keel/*_test.go`（参考 `plugin_prompt_test.go` 的装配测试）或 `cmd/e2e` 新 scenario | 工具面出现 app 工具（模型列）、app 目录可读、`/view` 返回带 CSP 的 HTML、`tools/call` 代理成功、`tools/list_changed` 后下一代工具面变化 |
| G2 | 崩溃一致：在「app 文档取回后 / tool.result 提交前 / listen 重连中」SIGKILL 并重启（`cmd/e2e/main.go` 既有 kill 框架，`scripts/acceptance-all.sh`） | `cmd/e2e` 扩展 scenario `mcp-apps` | 账本不丢/不重复；重启后 app 仍可挂载（绑定来自 `tool.result` details）；listen 重连后目录重算正确 |
| G3 | 可观测：`run.finished` 不变；新增指标固定基数——`mcp_servers{state}`、`mcp_surface_generation`、`mcp_app_fetch_total{result}`、`mcp_app_bridge_frames_total{method,direction}`、`mcp_listen_reconnects_total`、`mcp_mrtr_rounds`（直方图）；结构化日志字段含 `mcp_server`/`mcp_app`（**不含**消息正文） | `internal/mcphost` + `internal/httpapi/metrics.go` | 指标 series 数与会话数无关；审计行不含正文/凭据（扫描断言） |
| G4 | PI 对齐：`scripts/conformance.sh` 全绿；新增「真实 MCP server 零改配置可用」——用 ext-apps 仓的官方 example server（GitHub main 的 examples；若在 CI 不可直接安装，则用 §5.1 夹具并记录原因）作为真库夹具 | `scripts/` + `docs/PI-PARITY.md` | legacy 行为逐字节不回归；apps 元数据只在新路径生效 |
| G5 | 规模/运维：1000 会话 + 64 并发 run 下 `mcp_app` 目录与缓存不随会话增长（进程级）；2GB cgroup 内 20 个 app 实例 + 200 帧/s 桥流量；混沌 10 分钟随机 kill MCP server 与 listen 断流 | `scripts/acceptance-perf.sh` + 新 `scripts/mcp-apps-e2e.sh` | RSS 峰值 <1.8GB；断流重连 ≤ 退避上限；审计与诊断可见；报告落 `.agents/reports/`（主仓）并挂公网 |

### 5.3 浏览器 E2E 形态（Owner 的 E2E＝HTML 图文文档纪律）

1. `scripts/mcp-apps-e2e.sh`：启动 Postgres（或 `KEEL_TEST_DSN`）→ 起 `keel serve`
   + fake MCP server（stdio 与 HTTP 各一）→ 用 `keel tui`（或 web 目标）跑一次会话：
   「列出 app 工具 → 调用 `show_dashboard` → 断言内联 app 出现 → 在 app 内点按钮触发
   `tools/call refresh` → 断言结果回到 app → 点『发消息』→ 断言 decision 卡出现并发送 →
   断言 `user.message` 落账本、来源标记正确」；
2. 每步截图 + 步骤解说 → 单文件 `report.html`（before/after 或 after+断言），落位按主仓
   `.agents/reports/keel/mcp-apps-events/`，并挂公网（`pnpm tunnel:expose` 纪律在主仓侧）；
3. **降级面也要进 E2E**：TUI（不渲染 app，显示链接与文本兜底）、CSP 拒绝域名的 app
   （断言请求被浏览器挡下 + 宿主审计）、握手超时（注入挂死 View）各一格。

---

## 6. 分阶段任务拆解

> 每阶段有独立退出条件；P1 无 UI 也交付价值（元数据与注册面），P3 才是「看得见的 app」。
> 依赖顺序：P1 → P2 → P3 →（P4 可与 P3 并行）→ P5 → P6。

### P1 — 元数据、资源与 visibility（无 UI）

| 项 | 内容 | 文件 |
|---|---|---|
| 改动 | `Tool.Meta` 解析出 `_meta.ui{resourceUri,visibility}`（含废案平铺键兼容）；`AgentTools` 按 visibility 过滤并保留 app-only 工具到独立列表；`ListResources`/`ReadResource`/`ListPrompts`/`GetPrompt` 客户端方法；`MCPToolDetails` 增 `App`/`StructuredContent`；`tool.result` 的 details 透传断言；`mcp_apps` 提示段（工具带 UI 的摘要） | `internal/mcp/{protocol,tools,client,content}.go` |
| 测试 | 表驱动单测（visibility 过滤、废案键、任意 JSON structuredContent、分页资源）+ 现有 PI 移植测试全绿 | `internal/mcp/*_test.go` |
| 验收 | `go test ./internal/mcp/...`；`go vet ./...`；conformance 不变 |
| 风险 | 低。唯一注意：`structuredContent` 截断不许改动现有 20KB 文本裁剪的语义 |

### P2 — apps 宿主骨架与内联渲染

| 项 | 内容 | 文件 |
|---|---|---|
| 改动 | 新包 `internal/mcphost`（Surface/Sinks/Registry/AppSpec/资源缓存/CSP 构造/主题映射）；`mcpService` 经 sinks 首次发布（取代 `newPromptFactory` 的一次性读取）；`cmd/keel/serve.go` 装配 sinks（复用 `RefreshProductTools/Prompt`）；agentapi 路由 `GET /v1/mcp/servers`、`GET /v1/mcp/apps`、`GET /v1/mcp/apps/{id}/view`；`/v1/capabilities` 能力位 | `internal/mcphost/**`、`internal/mcp/manager.go`、`internal/servicehost/services.go`、`cmd/keel/serve.go`、`internal/agentapi/mcp_apps.go`、`internal/httpapi/server.go` |
| 前端 | `web/src/surfaces/mcp-app/{index,view,bridge}.tsx|ts`：iframe（`sandbox="allow-scripts"`）+ 握手 + tool-input/tool-result + 全屏；`messages.tsx` 的 `ToolRow` 挂载点 | `web/src/**` |
| 测试 | mcphost 单测（CSP/LRU/上限/hash）；桥单测；`cmd/keel` 装配测；E2E-1（内联渲染 + 断言 sandbox 属性、CSP 头） | 上述 + `cmd/keel` |
| 验收 | `go test ./...`；`nsh check`；`nsh test`；E2E 报告 P2 段 |
| 风险 | 中：「单 origin + opaque origin」的沙箱形态要一次做对（§3.7 deviation）；`ToolRow` 是高频渲染路径，app 实例必须按 `toolCallId` 复用（避免流式重渲染反复建 iframe） |

### P3 — 桥与出站动作

| 项 | 内容 | 文件 |
|---|---|---|
| 改动 | `POST /v1/mcp/apps/{id}/tools/call`、`/resources/read`（ownership 复核 + 审计）；`ui/message` → decision → 输入写路径；`ui/open-link` 确认 + `openUrl`；`ui/request-display-mode`；`size-changed`；`resource-teardown`；权限确认与偏好；审计表与读取；破坏性工具确认门 | `internal/agentapi/mcp_apps.go`、`internal/store/mcp_app_audit.go`、`web/src/surfaces/mcp-app/**` |
| 测试 | 越权/跨服务器调用拒绝；确认门（send/edit/discard 三分支）；scheme 白名单；超频/超帧断连；E2E-2（app 内调用 + 发消息 + 打开链接） |
| 验收 | 同上 + `GET /v1/mcp/apps/audit` 可见全部动作，且扫描断言无正文 |
| 风险 | 中：`ui/message` 的确认 UX 与 `delivery` 默认（queue）要与队列语义一致（`docs/CHAT-SURFACE.md`） |

### P4 — events 路由与订阅

| 项 | 内容 | 文件 |
|---|---|---|
| 改动 | `Dispatcher` 与通知分发（瞬态 vs 注册面）；`subscriptions/listen`（filter/ack/重连重发）；legacy GET 流保持；`tools/list_changed` 去抖 + 全量 republish + generation + `mcp.surface.changed` hook；`progress` → `PartialHub`（toolCallId 关联）；`notifications/message` → 日志；指标 | `internal/mcphost/events.go`、`internal/mcp/client.go`、`internal/mcp/streamable_http.go`、`internal/agentext/hooks.go`（事件名追加）、`internal/agentext/turns.go`（progress kind）、`internal/agent/runner.go`（ProgressToken 注册） |
| 测试 | 热更新 run 边界测（§4.4 第 7 条）；ack 子集缺项诊断；断线重连后目录重算；progress 丢帧无害；审计无正文 |
| 验收 | 上述 + 指标断言（基数固定） |
| 风险 | 中高：**发布路径只有一条**（sinks），必须与插件发布同锁/同失败语义（失败保留旧面）；progress 的会话关联要防串会话 |

### P5 — MRTR / 版本对齐 / app 上下文

| 项 | 内容 | 文件 |
|---|---|---|
| 改动 | `InputRequiredResult` 解析与 MRTR 执行器（elicitation→decision、sampling→模型服务+预算、roots→拒绝）；legacy 主动请求的等价路径 + 能力声明；era 探测与门控（session/Last-Event-ID/头部/`_meta`/`ttlMs`）；`ui/update-model-context` 的存储与 runner seam；`GET /v1/mcp/servers` 报 era/版本 | `internal/mcp/{protocol,client,streamable_http,stdio_transport}.go`、`internal/mcphost/events.go`、`internal/store/mcp_app_context.go`（新+迁移）、`internal/agent/runner.go`、`internal/servicehost/*` |
| 测试 | MRTR 端到端（含 requestState 篡改负控：服务器回新 inputRequests 而不报错）；轮次预算；`resultType` 三态；era 探测三分支；legacy 行为回归（PI 套件） |
| 验收 | `go test ./...` + conformance 全绿 + E2E-3（elicitation 卡答完，重试成功） |
| 风险 | 高：sampling 走当前会话模型＝隐式成本，必须默认受限并计量（与「价格只在合同侧、系统只计量」原则一致：只记客观用量，不写价格） |

### P6 — 验收与收口

| 项 | 内容 |
|---|---|
| 验收 | G0–G5 逐条证据（§5.2）＋浏览器 E2E 报告（HTML 图文）挂公网；两份 spec 与实现一致性复核（子代理交叉复核，本项目不用 Codex） |
| 文档 | `docs/HOST.md`（MCP 段：apps/events/era）、`docs/PI-PARITY.md`（偏离登记）、`site/docs/zh/harness/mcp-apps.md`（新增使用页）、`site/docs/harness/` 英文对照（若该站有英文面） |
| 收尾 | change 归档（交付即 archive）、主仓指针跟进（若在子仓工作）、`docs/RESIDUAL-PLAN.md` Wave 3.5 勾销 |

---

## 7. 关键风险与未决点

| # | 风险 | 影响 | 缓解 |
|---|---|---|---|
| R1 | **单 origin 沙箱 deviation** 被当作「不合规」 | 安全评审卡口 | §3.7 的等价约束 + `KEEL_MCP_APP_ORIGIN` 双 iframe 增强 + 明确登记（spec 是 SHOULD 级实现建议，安全意图（隔离/审计/同意）逐条满足） |
| R2 | **发布路径单点**：MCP 与插件若各自直接调用 `RefreshProductTools/Prompt`，任一侧发布空面会把另一侧刷掉 | 工具面瞬时缺失 | `cmd/keel/dynamic_surface.go` 聚合器（MCP 半面 ∪ 插件半面后一次性发布，§2.2）；失败保留旧面；加「发布组合」单测（插件刷新后 MCP 工具仍在，反之亦然） |
| R3 | **热更新与进行中 run** 的语义被误读为「立刻生效」 | 行为不可预测 | 测试固化「每 attempt 冻结」；`mcp.surface.changed` 只做通知不打断；文档写明 |
| R4 | **2026-07-28 stateless 改造**与 PI 基线冲突（PI 是 2025-11-25 形状） | PI 一致性回归 | era 门控 + 默认 auto + conformance 全绿为硬门；把「PI pin 升级」留给独立 change（与 Owner 决策 7「PI pin 跟随上游，每次升级独立 change」一致） |
| R5 | `ui/update-model-context` 需要 **DB 迁移 + runner seam**，是全案最重改动 | 排期与风险集中 | P5 单独阶段，能力位门控（未接线时 `-32601` + 能力位 false，spec 属 SHOULD）；替代方案（写账本 kind）显式否决并记录理由 |
| R6 | `sampling/createMessage` 经 MRTR 进入宿主，等同把「额外模型调用」的控制权交给第三方服务器 | 成本/安全 | 默认 `KEEL_MCP_SAMPLING=session`、`maxTokens` 上限、每调用预算、用户可见（审计 + UI 标记）、只计量不写价（与业务原则一致） |
| R7 | 桥是新的攻击面（帧洪水/协议违规/XSS 绕过） | 宿主 origin 安全 | opaque origin + 无 `allow-same-origin` + 白名单 + 限速限长 + 协议违规断连 + 审计；`/view` 路由的 CSP 头与安全头逐项照抄 `plugins_ui.go` 的纪律并加 `frame-ancestors` |
| R8 | ext-apps SDK 2.x 与 stable spec 正文的字段差异（`ui/message.content` 形状、`hostCapabilities` 子项） | 与真实 app 互通性 | 容忍两种形状（数组/对象）；只实现 spec 明确的面；SDK-only 方法（`ui/download-file`、`request-teardown`）回 `-32601` 并在 `/v1/mcp/servers` 的能力里说明（不假装支持） |
| R9 | apps 目录/缓存在 1000 会话规模下的内存 | G5 规模门 | 进程级单例（不随会话复制）+ 64 MiB 上限 LRU + 文档上限 2 MiB + 断言「目录大小与会话数无关」 |

**未决点（实现前需要一次对齐，默认值已给）**
1. `ui/message` 默认是否需要逐次确认（本设计默认「需要」，可用会话偏好关闭）——若 Owner 要
   「像 ChatGPT 一样直接进对话」，改动只是确认门默认值；
2. `KEEL_MCP_APP_ORIGIN` 是否在 GA 前提供第二 origin（本设计按「可选增强」排期，P6 前不做也行）；
3. progress 是否要在工具行显示进度条（本设计做了通道与指标，UI 呈现留 P4 末尾按可见性决定）。

---

## 附录 A：标准出处清单（抓取日 2026-11-14）

| # | 文档 | URL | 版本/日期 |
|---|---|---|---|
| 1 | MCP Apps SEP-1865（Stable） | `https://github.com/modelcontextprotocol/ext-apps/blob/main/specification/2026-01-26/apps.mdx` | Stable 2026-01-26；Created 2025-11-21 |
| 2 | MCP Apps（Draft） | `https://github.com/modelcontextprotocol/ext-apps/blob/main/specification/draft/apps.mdx` | Draft |
| 3 | MCP Apps 公告 | `https://blog.modelcontextprotocol.io/posts/2026-01-26-mcp-apps/` | 2026-01-26 |
| 4 | ext-apps SDK 包 | `https://github.com/modelcontextprotocol/ext-apps/blob/main/package.json`；`https://registry.npmjs.org/@modelcontextprotocol%2Fext-apps/latest` | main `2.1.0`；npm latest `2.0.4` |
| 5 | ext-apps 生成 schema（方法名/字段权威） | `https://github.com/modelcontextprotocol/ext-apps/blob/main/src/generated/schema.json` | 与 main 同批（含 `ui/download-file` 等 SDK 新面） |
| 6 | MCP 2026-07-28 Key Changes | `https://modelcontextprotocol.io/specification/2026-07-28/changelog`（源 `docs/specification/2026-07-28/changelog.mdx`） | 2026-07-28 |
| 7 | Subscriptions（`subscriptions/listen`） | `…/2026-07-28/basic/patterns/subscriptions.mdx` | 2026-07-28 |
| 8 | Multi Round-Trip Requests | `…/2026-07-28/basic/patterns/mrtr.mdx` | 2026-07-28 |
| 9 | Streamable HTTP | `…/2026-07-28/basic/transports/streamable-http.mdx` | 2026-07-28 |
| 10 | Base protocol（`_meta`/`resultType`/错误码） | `…/2026-07-28/basic/index.mdx` | 2026-07-28 |
| 11 | 上一版（keel 当前基线） | `https://modelcontextprotocol.io/specification/2025-11-25` | 2025-11-25 |

## 附录 B：实现速查（不做推断，逐字使用）

- 通知：`notifications/progress`、`notifications/message`、`notifications/tools/list_changed`、
  `notifications/prompts/list_changed`、`notifications/resources/list_changed`、
  `notifications/resources/updated`、`notifications/cancelled`、
  `notifications/subscriptions/acknowledged`。
- 请求：`tools/list`、`tools/call`、`resources/list`、`resources/read`、
  `resources/templates/list`、`prompts/list`、`prompts/get`、`subscriptions/listen`、
  `server/discover`（modern）、`initialize`/`notifications/initialized`（legacy）。
- `_meta` 键：`io.modelcontextprotocol/protocolVersion`、`…/clientInfo`、`…/clientCapabilities`、
  `…/serverInfo`、`…/subscriptionId`、`…/logLevel`。
- 头：`MCP-Protocol-Version`、`Mcp-Method`、`Mcp-Name`、`Mcp-Session-Id`（legacy）、
  `Last-Event-ID`（legacy）。
- apps 方法：见 §0.1 的 17 个 `ui/*`；字段：`_meta.ui{tool:{resourceUri,visibility},
  resource:{csp{connectDomains,resourceDomains,frameDomains,baseUriDomains},
  permissions{camera,microphone,geolocation,clipboardWrite},domain,prefersBorder}}`。
