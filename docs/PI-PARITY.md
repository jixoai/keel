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
| **宿主** | 提示词装配、skills、扩展事件、命令、UI | `packages/coding-agent` | `internal/httpapi` + 未来的 `internal/host` |
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
- 工具适配：`mcp__<server>__<tool>` 命名、`normalizeMcpInputSchema`、`toLlmContent`、
  20KB 输出裁剪、progress 透传；
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
| MCP | `McpClient` + 两种传输 + 命名/裁剪 | `internal/mcp` | 真实 MCP server 零改动 |
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

## 5. 版本与升级

- `PI_PIN` 写入本文件与 `docs/UPSTREAM.md`；升级前先跑 conformance suite。
- 任何「PI 没有、我们想加」的能力，先判断能否用现有扩展点表达；不能才立项，
  并在 `docs/MATURITY.md` 标注为 Keel extension，不冒充 PI 标准。

## 6. 对当前计划的修正

1. **Skills 不进内核**：`internal/agent` 不感知 skill；由宿主层装配系统提示 + `skill` 工具。
2. **MCP 独立包**：`internal/mcp` 镜像 `pi-mcp` 的传输/命名/裁剪/OAuth 语义，再适配 `AgentTool`。
3. **Hooks 用 PI 事件名**：`before_agent_start`、`turn_start/end`、`tool_execution_*`、`context`、
   `session_before_compact` 等，不自造 `tool.before` 这类新名字。
4. **遥测用 PI 的 span/metric 契约**：先做 `TelemetryContext`/`Span`/`Metrics` 接口与
   noop/memory adapter，再接 Prometheus/OTLP；`run.finished` 只作为业务汇总事件。
5. **压缩复用 PI durable 语义**：`CompactionEntry`/`CompactionCheckpoint`/policy 默认值对齐。
