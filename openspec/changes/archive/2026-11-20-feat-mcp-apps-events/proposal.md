# Proposal: feat-mcp-apps-events

> 立项：Owner 2026-11-14「MCP App 和 MCP events，这两个标准也要想办法对接上」。
> Keel 在 MCP 里是 **host/client**；本 change 只做 host 侧接入，不做 server 侧。
> 设计先行（`design.md`），实现排 `docs/RESIDUAL-PLAN.md` Wave 3.5/4 的 mcp 波。

## Why

Keel 现在的 MCP 客户端是一个**工具搬运工**：连接（stdio / Streamable HTTP）→ `tools/list` →
把每个工具适配成 `agentcore.AgentTool`（`internal/mcp/tools.go:38`）→ 交给宿主工具注册表。
两个官方标准面完全没接：

1. **MCP Apps**（官方第一个扩展，SEP-1865，扩展标识 `io.modelcontextprotocol/ui`，
   **Stable 2026-01-26**，延续进 2026-07-28 核心修订）已经能用了：工具用
   `_meta.ui.resourceUri` 指向一个 `ui://` 资源（MIME `text/html;profile=mcp-app`），宿主
   取回 HTML、在沙箱 iframe 里渲染，用 postMessage 上的 JSON-RPC 双向通信
   （`ui/initialize` → `ui/notifications/initialized`；数据向下
   `ui/notifications/tool-input` / `tool-result`；动作向上 `tools/call`、`ui/message`、
   `ui/open-link`、`ui/update-model-context`）。今天的 keel：`Tool` 结构里的 `_meta` 已经被
   读进来了（`internal/mcp/protocol.go:402`）但**没人看**；`ReadResourceResult`、
   `Resource`、`Prompt` 类型与 `ServerCapabilities.Resources` 都在（protocol.go:355-463），
   却没有 `resources/list` / `resources/read` / `prompts/*` 的客户端方法。`_meta.ui.visibility`
   的语义（`["app"]` 的工具**不得**出现在模型面）也没有任何过滤器——今天所有工具都进模型面
   （`internal/mcp/tools.go:38-64`），这在标准里是错的。

2. **MCP events** 的面也是空的。官方没有独立「Events 规范」：事件面由 Transports 规范承载
   ——`notifications/progress` / `notifications/message` / `notifications/{tools,prompts,
   resources}/list_changed` / `notifications/resources/updated`，以及 2026-07-28 修订新增的
   **`subscriptions/listen`**（单一长连接 POST-response 流，客户端按类型 opt-in，
   服务端以 `notifications/subscriptions/acknowledged` 应答并用
   `io.modelcontextprotocol/subscriptionId` 标注每条通知）。今天 keel 的接收循环**丢弃所有
   通知**（`internal/mcp/client.go:387`「Notifications are not consumed」），
   服务端发来的请求一律回 `method not found`（client.go:411）。于是：`tools/list_changed`
   会让注册面长期停留在 boot 时的快照；`progress` 到不了 UI；而 2026-07-28 的
   **MRTR**（Multi Round-Trip Requests：服务端用 `InputRequiredResult`
   `resultType:"input_required"` + `inputRequests` + `requestState` 请求补充信息，客户端带
   `inputResponses` 重试）取代了服务端主动请求——keel 今天既没有 `resultType` 容忍（旧服务端
   缺字段必须当 `"complete"`），也不认 `input_required`，遇到就会把结果当普通成功。

缺口合并成一句话：**keel 只能吃 MCP 的「工具」一层，而且只吃一次（boot）。**

## What Changes

### 1. MCP 变成声明式注册面（依赖即注册，Owner 2026-11-14 架构第一原则）

MCP 从「一个 loadout 服务」升级为**向宿主注册四类能力**的注册者，与 `agentext` 插件注册面
同构（工具 / 提示段 / 资源 / apps / events），全部走既有的「每次装配全量发布（republish）」
语义，而不是新增私有通道：

- 工具（既有）：`manager.Tools()` → 宿主注册表（`internal/servicehost/services.go:205`）；
- 提示段（既有）：`manager.PromptSection()` → `host.Section{Name:"mcp_servers"}`
  （services.go:206-208）；
- 资源（新）：服务器声明的 `Resource` 目录 + 按需 `resources/read`，成为宿主可查询的资源面；
- apps（新）：工具上的 `_meta.ui` 与 `ui://` 资源成为**可渲染的结果面**；
- events（新）：通知按类型路由——瞬态 UI 状态 vs 注册面热更新 vs 版本协商。

### 2. MCP Apps 宿主

`ui://` 资源在宿主侧取回、缓存、按 app 化 ID 托管；web 客户端在**工具结果卡内**渲染
（内联）并可全屏；postMessage 上跑 JSON-RPC 桥，`tools/call` 由**宿主代理**（不是 iframe
直连服务器）、`ui/message` 落 composer/队列（默认经既有 decision 原语确认）、
`ui/update-model-context` 落会话上下文、`ui/open-link` 走显式确认。沙箱与 CSP 是 requirement，
不是注意事项。

### 3. MCP events 接入

通知分类落点明确：`progress`/`message` 是瞬态（不落账本）；`*/list_changed` 与
`resources/updated` 触发**注册面热更新**（复用 `Runtime.RefreshProductTools/RefreshProductPrompt`
的 republish 语义，进行中的 run 不受影响）；`subscriptions/listen` 由宿主按服务器订阅、
断线重发（2026-07-28 无回放，只有重订阅）；MRTR 的 `inputRequests`（elicitation / sampling /
roots）映射到既有的 decision / consent 与模型服务，`requestState` 原样回显、有轮次预算。

### 4. 版本对齐（不破坏 PI 基线）

`internal/mcp` 是 PI `@earendil-works/pi-mcp` 的行为移植（`docs/UPSTREAM.md`），当前
`LatestProtocolVersion = "2025-11-25"`（protocol.go:24）。2026-07-28 移除了 session、
`Last-Event-ID` 恢复、GET 流与 `initialize` 握手，改为每请求 `_meta` + `server/discover` +
`subscriptions/listen`。本 change **不**改默认版本，而是在同一客户端内做**双形态（era）**支持：
legacy（≤2025-11-25，含 PI 一致性套件覆盖的行为）与 modern（2026-07-28），
新增 `SupportedProtocolVersions` 条目、按版本门控的传输行为、`resultType` 容忍与
`InputRequiredResult` 处理。PI 一致性 `scripts/conformance.sh` 必须保持全绿。

## Non-Goals

- **不做 MCP server 侧**（不发布 keel 自己的 apps/resources/prompts；`internal/gateway`
  是私有 DSH 协议，不在本 change 范围）。
- **不实现 ext-apps SDK 本身**：`@modelcontextprotocol/ext-apps` 是服务器/视图作者的 TS 资产；
  keel 只实现宿主侧线协议，视图作者可以自带 SDK。我们自己的 web 宿主实现**不引入该 SDK**
  （零新前端依赖；协议面小且有官方 schema 可对表）。
- **不做 OpenAI Apps SDK / MCP-UI 的私有方言**（不实现 `window.openai`、MCP-UI 的
  `ui/notifications/iframe-ready` 等前身协议）。
- **不把 MCP 通知写进事件账本**：账本是**会话事实**（`internal/kernel/kernel.go:74-96` 的
  封闭 kind 表），服务器目录变化不是会话事实；本 change 明确「哪些落账本、哪些只做瞬态/注册面」，
  不为 MCP 新增 kernel kind。
- **不实现 `ui/notifications/tool-input-partial`**（需要 provider 层工具参数增量投影，keel 今天
  没有；视图 MUST 忽略即可）。
- **不支持 `pip` 显示模式**（宿主只声明 inline + fullscreen）。
- **不做远程资源订阅的跨会话共享**：资源缓存是**服务器作用域**，但只服务本进程；不引入分布式
  缓存或跨 runtime 复制。
- **不做 app 商店/审批目录**：apps 随 MCP 服务器配置出现，不引入第二个安装审批面（插件审批
  体系不变）。

## Impact

- **协议面**：`internal/mcp`（client/transport/protocol）、新包 `internal/mcphost`（app 与事件
  路由）。
- **注册面**：`internal/servicehost`（republish 路径复用）、`internal/agentext`（新 sinks 与
  规格对照）。
- **HTTP 面**（agentapi）：新增 `/v1/mcp/apps*`（目录、文档、代理 tools/call、
  `resources/read`）、`/v1/mcp/servers*`（状态与变更身份）；`/v1/capabilities` 增
  `mcp.apps`、`mcp.apps.sandboxOrigin`、`mcp.events` 能力位。
- **前端**：新目录 `web/src/surfaces/mcp-app/`（bridge + view + 沙箱代理），
  改动 `web/src/screens/messages.tsx`（工具结果卡挂载点）与 `web/src/data/*`（能力位、
  app 目录读取）；不新增 UI 组件（只用 kit）。
- **DB**：仅 P5 的 app 上下文（`mcp_app_contexts`）需要迁移；app 目录/缓存是进程内 +
  服务器作用域的审计行。
- **测试**：fake MCP server（stdio + streamable HTTP 双传输）成为
  `internal/mcp` / `internal/mcphost` 的常驻夹具；新增浏览器 E2E 与 G0–G5 逐条证据。
- **文档**：`docs/PI-PARITY.md` 登记偏离（MCP Apps/events 是 PI 没有的面）、
  `site/docs/zh/harness/` 增 MCP Apps 使用页、`docs/HOST.md` 的 MCP 段更新。
