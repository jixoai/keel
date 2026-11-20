# PI 对齐基线（PI Parity）

> **Keel 是 PI 的 Go 重构，不是另起一套标准。**
> 接口、事件语义、产物格式以 PI 为唯一参照；Keel 只替换运行时（无状态 / PostgreSQL /
> 多租户 / 2GB / SSE），不改变对外契约。

- 上游：`earendil-works/pi`
- Pin：`55a5c6914570e2ae31ca36297654483bbbf46883`（2026-11-05）
- 参照包：`packages/agent`、`packages/durable`、`packages/mcp`、`packages/telemetry`、
  `packages/coding-agent`（skills / extensions / compaction）

## 1. 三层结构：内核 / 宿主 / 协议

PI 的分层是这次重构最重要的约束：

| 层 | 职责 | PI 位置 | Keel 对应 |
|---|---|---|---|
| **内核** | 循环、事件流、工具契约、生成/压缩任务 | `packages/agent`、`packages/durable` | `internal/loop`、`internal/agentcore`、`internal/agenttool` |
| **宿主** | 提示词装配、skills、扩展事件、命令、UI | `packages/coding-agent` | `internal/host` + `internal/httpapi` |
| **协议** | MCP、遥测、provider 文件 | `packages/mcp`、`packages/telemetry`、`packages/ai` | `internal/mcp`、`internal/telemetry`、`internal/provider` |

**判定规则**：一个能力如果需要新的**执行原语/协议/传输**，进内核或独立协议包；
如果只是**指令、内容、提示词、流程**，进宿主层或扩展，不进内核。

## 2. Skills 与 MCP 为什么位置不同

### Skills = 内容，不是协议

PI 的核心 `packages/agent/src` **没有任何 skill 引用**；skills 只在
`packages/coding-agent/src/core/skills.ts` 实现，遵循
[Agent Skills 规范](https://agentskills.io/specification)：

- 一个目录 + `SKILL.md`（frontmatter：`name`/`description`/`license`/`compatibility`/
  `metadata`/`allowed-tools`/`disable-model-invocation`）；
- 启动时只把 name + description + path 放进系统提示；正文按需读取（渐进披露）；
- `/skill:name` 强制加载；skill 可带 scripts/references/assets，但**没有新的执行原语**；
- 位置：`~/.agents/skills/`、`.agents/skills/`、用户/项目 skills 目录。

所以 skills 属于**宿主层**：它改变的是「模型看到什么」，不是「运行时能执行什么」。
把它塞进内核会让内核绑定某一种内容格式，违背 PI 的小内核设计。

### MCP = 协议边界，必须是独立客户端

PI 的 MCP 是独立包 `@earendil-works/pi-mcp`，**不依赖官方 MCP SDK**：

- 传输：stdio、Streamable HTTP（明确拒绝 legacy SSE）、PipeTransport、内存测试传输；
- 客户端职责：JSON-RPC 关联、初始化/能力协商、超时、取消、server 请求、OAuth 子集；
- 工具适配：PI 的 `mcp__<server>__<tool>` 命名、`normalizeMcpInputSchema`、`toLlmContent`、
  20KB 输出裁剪、progress 透传；Keel **有意偏离命名**（见 §4.2），其余语义保留；
- 宿主职责：`mcp.json` 配置、项目信任、exposure、`/mcp` 管理。

这些是**线协议与生命周期**，无法用提示词表达；每个宿主都需要同一套客户端。所以它是
一等包，而不是扩展。**一句话：PI 把「意义」放进提示词/宿主扩展，把「边界」放进协议包。**

## 3. 对齐矩阵（必须逐项有 conformance test）

| 能力 | PI 契约 | Keel 目标 | 验收 |
|---|---|---|---|
| 循环 | `AgentLoopConfig`、`PrepareRequest`、`AgentTurnDecision`、`Before/AfterToolCall` | `loop.RunConfig` 同名语义 | 事件序列回放一致 |
| 事件 | `AgentEvent`（`agent_start`/`turn_start`/`message_*`/`tool_execution_*`/`agent_end`） | 同名事件、同顺序 | 用 PI 录制流做 golden test |
| 工具 | `AgentTool`（name/description/parameters/execute/executionMode） | `agentcore.AgentTool` 同形 | 同一工具描述在两端可互换 |
| 持久任务 | `GenerationTask`、`CompactionTask`、restart/retry/progress policy | `tasks` 表 + 同语义 | kill 矩阵 |
| 压缩 | `CompactionEntry`、`CompactionCheckpoint`、`DEFAULT_COMPACTION_POLICY` | `compaction` 事件 + checkpoint | 摘要/保留段/token 账一致 |
| 扩展 | `defineExtension`、`hook`、`section`、`wrapTool` | Go 接口 + 事件名对齐 | 同一 hook 语义 |
| Skills | Agent Skills 规范 + `skills.ts` | 宿主层 loader | 真实 SKILL.md 零改动 |
| MCP | `McpClient` + 两种传输 + 命名/裁剪 + OAuth（discovery/DCR/CIMD/PKCE/sign-in） | `internal/mcp`、`internal/mcp/oauth` | 真实 MCP server 零改动 |
| 遥测 | `TelemetryContext`/`TelemetrySpan`/`TelemetryStore` + adapters | 同形接口 + Prometheus/OTLP adapter | 同一 span 树 |
| Provider | `StreamFn`、`AssistantMessageEvent`、`ImageContent` | 已搬运 | 协议驱动器回放 |

## 4. 允许的差异（运行时替换，不是契约改变）

- 存储：PI 的 JSONL/SQLite → PostgreSQL 事件表 + checkpoint；
- 传输：PI 的本地进程 → HTTP/SSE + 多租户；
- 并发：单会话串行不变，跨会话用租约 + `FOR UPDATE SKIP LOCKED`；
- 部署：2GB 预算、反压、分块 blob；
- 不含：TUI/CLI/REPL、主题、终端渲染、npm 包体系。

**不允许的差异**：事件名与顺序、工具接口形状、SKILL.md 格式、MCP 配置/命名/传输语义、
压缩 entry/checkpoint 语义、telemetry span/metric 形状。

> 例外只有已登记的有意偏离（§4.2）。MCP 的**传输与配置语义**仍与 PI 对齐；偏离限于
> 宿主如何把 MCP 工具暴露给模型。

## 4.1 官方测试对齐状态

逐文件对照、已移植测试与未覆盖项见 [PI-CONFORMANCE.md](PI-CONFORMANCE.md)。
当前：全仓 `go test ./...` 通过；覆盖 loop / compaction / MCP（含 stdio、Streamable HTTP、
配置、OAuth 授权与 sign-in）/ skills / telemetry / hooks / durable / host / runner。
MCP OAuth 的运行时代理层（PostgreSQL 加密存储 / 独立锁池刷新锁 / HTTP sign-in /
同进程重连）见 `internal/mcp/oauth` 与 `internal/store/mcp_oauth.go`。

## 4.2 有意偏离：MCP 工具命名（keel-harness-v2）

| 项 | 内容 |
|---|---|
| PI 契约 | 每个 MCP tool 适配为一个模型可见工具，名字 `mcp__<server>__<tool>`；server 增删会改变工具清单 |
| Keel 实现 | 唯一模型可见入口是恒定内建工具 `mcp`；模型用 `mcp {action:"list" / "describe" / "call", server, tool, arguments}` 访问。**不生成**任何 `mcp__<server>__<tool>` 名字（无 shim/别名/双注册） |
| 理由 | 恒定工具面是 DI 注册与可替换性的前提：工具清单只随**安装审批过的** `provides.tools` 变化，不随接入的 MCP server 数变化；`toolManifestHash` 因此可稳定回答「这次 run 有没有新工具」 |
| 影响面 | 模型提示与工具搜索不再看到 per-server 工具名；需要 per-tool 声明（如 allow-list）的部署改按 `mcp` + server/tool 参数表达；MCP server 的 wire 协议、配置、传输、内容转换、20KB 裁剪全部不变 |
| 验收方式 | ① 终局工具集快照断言：内建精确 11 个且 `mcp__` 精确名零出现（`internal/toolsurface`）；② 清单哈希：挂 100 个 MCP server 哈希不变、审批插件 `provides.tools` 变化/卸载恢复；③ E2E wire 断言：captain/member 请求的工具名精确等于 `agent` / `clock`，全量请求无 `mcp__`/`creation_`/`spawn_subagent`/`skill` |
| 不成立的论据 | 不采用「旧适配从未生效」：v1 已实现并测试过 `mcp__<server>__<tool>`；本偏离是 Owner 2026-11-11 破坏性迁移裁决 |

## 4.3 有意偏离：MCP Apps 与 events（keel extension，PI 没有的面）

MCP Apps（SEP-1865，`io.modelcontextprotocol/ui`）与 events 面（`subscriptions/listen`、
`resultType`、MRTR 等 2026-07-28 修订）是 **PI 没有的能力**。keel 以扩展方式接入，
不冒充 PI 标准；legacy MCP 的工具适配、传输、配置、内容转换与 20KB 裁剪逐条不变。
逐条对表见 `openspec/changes/archive/2026-11-20-feat-mcp-apps-events/evidence/p6/docs/parity.md`。

| 偏离/扩展 | 依据 | 影响面 | 替代/回退 |
|---|---|---|---|
| **恒定 `mcp` 聚合工具**（本 change 的前置契约） | §4.2：PI 的 `mcp__<server>__<tool>` 命名会让工具清单随接入服务器数变化，破坏 DI 注册与 `toolManifestHash` 的稳定性；Owner 2026-11-11 裁决 | 模型面只有 11 个内建工具；MCP 工具经 `mcp {action:"list" / "describe" / "call"}` 访问；app-only 工具不进模型面 | 无回退开关；需要 per-server 工具名的部署改按 `mcp` + server/tool 参数表达（§4.2 的验收方式不变） |
| **现代 era + MRTR**（2026-07-28） | PI 基线是 2025-11-25；现代修订移除 `initialize`/session/GET 恢复，引入 `server/discover`、`resultType`、`subscriptions/listen` 与 MRTR。keel 在同一客户端内做双形态，默认 `auto` | legacy 行为逐字不变（PI conformance 1071/0/1）；modern 下无 session/`Last-Event-ID`，每请求 `_meta` + `Mcp-Method`/`Mcp-Name`；`input_required` 走 decision/sampling/roots 拒绝 | `KEEL_MCP_PROTOCOL_ERA=legacy` 强制旧线协议；不追求与 PI 同版本（PI pin 升级留给独立 change） |
| **Apps host proxy**（View 不直连服务器） | Apps 的安全叙事要求可审计通信 + 用户同意；服务器凭据只在宿主；View 是第三方 HTML。PI 没有 apps 面可复用 | `tools/call`/`resources/read`/`ui/message`/`ui/open-link`/`context` 全走 `/v1/mcp/apps/{id}/*`；服务器由目录派生；破坏性工具与消息/链接有 confirmation；审计落 `mcp_app_audit` | 不支持 apps 的客户端/TUI 自动走文本（标题 + 链接 + 结果）；没有「页面直连 MCP」的回退路径（那会绕过审计与授权） |
| **Sandbox 第二 origin 暂缓**（spec 的可选增强未落地） | spec 允许 `mcp.apps.sandboxOrigin`；实现受 NSH 0.4 无 `sandbox` prop、单 origin 部署限制。当前 `KEEL_MCP_APP_ORIGIN` 只作为桌面/移动 WebView 的绝对文档基址；web 始终同源 opaque frame；**没有 sandbox 代理页 + 内层 View 的双 iframe** | `mcp.apps.sandboxOrigin` 只表示「配置了原生宿主基址」，不表示双 iframe；未配置时 web 仍可用，原生显示「未配置沙箱域」文本 | 单 origin 等价约束：iframe 无 `allow-same-origin`（opaque origin）+ 文档 CSP `sandbox allow-scripts` + `frame-ancestors 'self'` + 无凭据跨源 fetch 失败；增强留 P6 之后独立项（tasks 已知暂缓 c） |
| **`ui/download-file` / `ui/notifications/request-teardown` 不实现** | 两者是 ext-apps SDK 2.x 新面，不在 stable 2026-01-26 正文；keel 不引入 SDK，避免零新前端依赖 | `ui/download-file` 回 `-32601`；`request-teardown` 是通知（无 id），按未知通知 → 协议违规销毁实例；不静默吞掉、不假装支持 | 无；View 应按 spec 做特性检测并忽略未声明能力 |
| **P3.6 权限依赖 NSH allow** | camera/microphone/geolocation/clipboardWrite 的授予需要 iframe `allow` 属性与 per-permission 偏好；NSH 0.4 WebView 没有该控制面，单 origin 沙箱也无 `allow-same-origin` | `_meta.ui.permissions` 会解析并随目录发布，但 `hostCapabilities` 不声明 `sandbox.permissions`、iframe 无 `allow`；当前**不授予**任何浏览器权限，拒绝记忆也无从生效 | 应用必须特性检测（无权限时降级）；这是上游依赖，不声明即不假装。若必须授予，需升级 NSH 或落第二 origin 方案 |
| **`ui/notifications/tool-input-partial` / `pip` 不实现** | design §3.5 Non-Goal：keel 没有 provider 级工具参数增量投影；宿主只声明 inline + fullscreen | 不发 partial；`pip` 请求回当前实际模式（inline/fullscreen）；View 按 spec MAY 忽略 | 无；不影响 tool-input/tool-result 主路径 |

**P6 已收口（2026-11-20）：**

- **MCP Apps 客户端能力声明**：`ClientCapabilities.Extensions` + `DeclareMCPApps()` 在
  `extensions`/`experimental` 双位置写入相同 `{"mimeTypes":["text/html;profile=mcp-app"]}`，
  仅在 Registry 接线时声明（`internal/mcp/protocol.go`、`internal/servicehost/services.go`；
  测试 `TestInitializeDeclaresMCPAppsInBothPositions`）。
- **`_meta.ui.domain`**：`AppSpec.Domain` 投影到目录，前端 `McpAppView.domain` 透传
  （`internal/mcphost/registry.go`、`web/src/data/mcp-apps.ts`；测试
  `TestRegistryPublishesValidatedApps`、`web/src/data/mcp-apps.test.ts`）。
- **`tool-input` 实参**：聚合 `mcp` 信封在前端解包为底层工具实参（`mcpAppToolInput`）。
- **pre-load `ui/initialize`**：首载不再被误判为自导航（`frame-load.ts`）。

**已知未实现（不是有意偏离，需后续 change 收口）：**

- **`ui/resource-teardown` 等待不阻塞（已接受）**：`web/src/surfaces/mcp-app/view.tsx`
  的卸载路径调用 `bridge.teardown()` 后立即 `destroy()`；1 s 有界等待只在桥单测覆盖。
  NSH 0.4 的组件卸载在同一同步批内移除 iframe，无法延迟卸载而不冻结界面；记录为已接受
  偏离（`evidence/p6/fix/notes.md` §3），无数据/安全/账本影响。
- **MRTR 轮次不可配置**：`internal/mcp/client.go` 固定 `defaultMRTRMaxRounds = 5`；
  `design.md` §4.6 提到的 `KEEL_MCP_MRTR_MAX_ROUNDS` **没有实现**（`CallOptions` 可按调用
  覆盖，但没有环境变量）。冻结 spec 只要求默认预算，不算 spec 偏离；文档不得把它写成可配。

## 5. 版本与升级

- `PI_PIN` 写入本文件与 `docs/UPSTREAM.md`；升级前先跑 conformance suite。
- 任何「PI 没有、我们想加」的能力，先判断能否用现有扩展点表达；不能才立项，
  并在 `docs/MATURITY.md` 标注为 Keel extension，不冒充 PI 标准。

## 6. 对当前计划的修正

1. **Skills 不进内核**：`internal/agent` 不感知 skill；由宿主层装配系统提示 + `skill` 工具。
2. **MCP 独立包**：`internal/mcp` 镜像 `pi-mcp` 的传输（stdio / Streamable HTTP）/命名/裁剪/`mcp.json`/AuthProvider 语义，再适配 `AgentTool`。
3. **Hooks 用 PI 事件名**：`before_agent_start`、`turn_start/end`、`tool_execution_*`、`context`、
   `session_before_compact` 等，不自造 `tool.before` 这类新名字。
4. **遥测用 PI 的 span/metric 契约**：先做 `TelemetryContext`/`Span`/`Metrics` 接口与
   noop/memory adapter，再接 Prometheus/OTLP；`run.finished` 只作为业务汇总事件。
5. **压缩复用 PI durable 语义**：`CompactionEntry`/`CompactionCheckpoint`/policy 默认值对齐。
