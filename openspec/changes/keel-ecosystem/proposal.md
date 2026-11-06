# Proposal: keel-ecosystem

> 立项：Keel 首版与 Agent 执行落地后（2026-11-06）。
> **本 change 的定位是「把 PI 的接口标准与生态标准接进 Keel」——不是另起一套标准。**
> 验收以 `docs/MATURITY.md` 的 G0–G5 为准，G4 明确为 **PI 对齐**。

## 0. PI 对齐基线

- 上游：`earendil-works/pi` @ `55a5c69`（2026-11-05）。
- 三层结构：**内核**（loop/events/tools/durable）、**宿主**（skills/extensions/commands/UI）、
  **协议**（MCP/telemetry/provider files）。
- 判定规则：需要新执行原语/协议/传输 → 内核或独立协议包；只是指令/内容/流程 → 宿主层。
- 详细矩阵与 conformance 计划见 `docs/PI-PARITY.md`。

## Why

Keel 已有：事件日志、SSE 续传、流式 blob、反压、持久 run、工具执行、崩溃恢复。
缺的是 PI 已经把「Agent 该长什么样」沉淀下来的那部分：可观测、可恢复的完整语义、
上下文压缩、skills 宿主、MCP 协议客户端、扩展 hook。社区实现很多，但大多是单机、
有状态、绑定某个宿主；Keel 要按 PI 的契约在无状态/多租户/2GB 下重写。

## What Changes

### 1. `observability`（P1，关键路径起点）

- **先实现 PI telemetry 契约**：`TelemetryContext` / `TelemetrySpan` / `TelemetryMetrics` /
  `TelemetryStore`，以及 `noop` / `memory` 两个 adapter（与 `packages/telemetry` 同形）。
- run = 一个 span；turn / tool / compaction = 子 span；`message_*` 作为 span event。
- 业务汇总事件 `run.finished`（turns/tokens/toolTimings/context/model/duration/stopReason）
  可追加到事件日志，但**不进入模型上下文**；它不是 PI 契约的替代。
- Prometheus 文本指标作为 adapter 输出；OTLP 可选（默认关）。
- 日志：request/tenant/session/run id；**不记录消息正文与密钥**。
- **G3 硬约束**：指标 series 不随会话数增长；禁止 session/run id 标签。

### 2. `recoverability`（P1，与 1 并行）

- 对齐 PI durable 的 `GenerationTask` / `CompactionTask` / retry/progress policy 语义。
- **generation 幂等**：模型请求带 `generation_id`；恢复时若该 id 的 assistant 事件已存在，
  MUST 复用，不再请求模型。
- **tool 结果唯一**：`(tenant_id, session_id, tool_call_id)` 唯一；重复执行只留一条结果。
- checkpoint 回放：`CompactionEntry` / `CompactionCheckpoint` 语义对齐；恢复 = checkpoint + tail。
- 崩溃矩阵五格：模型流 / 工具 / 追加 / 压缩 / SSE；nightly 10 分钟随机 kill。

### 3. `compaction`（P1，依赖 2）

- 对齐 PI：`CompactionEntry`、`CompactionCheckpoint`、`DEFAULT_COMPACTION_POLICY`。
- 阈值 → 切点（保留最近 N token）→ 摘要 → 追加 `compaction` 事件 + checkpoint →
  用摘要 + 保留段重建；历史事件永远可读。
- 不在工具调用中间压缩；不压缩最后一条 user；摘要失败保留原上下文。
- 手动 `POST /v1/sessions/{id}/compact`。

### 4. `skills`（P2，宿主层，内核不感知）

- 遵循 **Agent Skills 规范**（与 PI `coding-agent/src/core/skills.ts` 同源）：
  `SKILL.md` frontmatter `name` / `description` / `license` / `compatibility` / `metadata` /
  `allowed-tools` / `disable-model-invocation`。
- 启动只放 name + description + path 到系统提示；正文按需加载（渐进披露）；
  `/skill:name` 强制加载（与 PI 一致）。
- 目录：`~/.agents/skills/`、`.agents/skills/`、项目/用户 skills 目录。
- 租户隔离与版本：内容 hash 去重、版本不可变；同名冲突保留首个并告警（PI 语义）。
- **内核不感知 skill**：装配发生在宿主层；`internal/agent` 只看到 sections + tools。
- 脚本/资源默认不执行；需要显式权限（见 Open Decision 1）。

### 5. `mcp`（P3，独立协议包，镜像 `@earendil-works/pi-mcp`）

- 传输：**stdio + Streamable HTTP**；**拒绝 legacy SSE**（与 PI 一致）；`PipeTransport`
  与内存测试传输。
- 客户端：JSON-RPC 关联、初始化/能力协商、超时（默认 60s）、取消、server 请求、
  OAuth 子集（`@earendil-works/pi-mcp/oauth` 语义）。
- 工具适配：`mcp__<server>__<tool>`（非法字符替换 + 冲突 hash 后缀 + 64 字符上限）、
  `normalizeMcpInputSchema`、`toLlmContent`、20KB 输出裁剪、progress 透传。
- 租户配置：`mcp.json` 兼容格式（`mcpServers`、`command/args/env/cwd`、`url/headers/oauth`、
  `timeout`、`enabled`、`exposure`）；凭据加密、日志脱敏。
- 安全：破坏性工具默认需 hook/审批；server 崩溃不拖垮 run。

### 6. `hooks`（P2，事件名对齐 PI 扩展模型）

- 事件名直接采用 PI `extensions.md` 的命名，不自造新名字：
  `before_agent_start`、`agent_start` / `agent_end` / `agent_before_settle` / `agent_settled`、
  `turn_start` / `turn_end`、`message_start` / `message_update` / `message_end`、
  `tool_execution_start` / `tool_execution_update` / `tool_execution_end`、
  `context`、`session_before_compact` / `session_compact` / `session_compact_failed`。
- 两种实现：进程内 Go 接口（对齐 `defineExtension` / `hook` / `section` / `wrapTool` 语义）
  与命令钩子（JSON stdin/stdout；超时 5s；失败策略可配）。
- 决策：`continue` / `deny` / 修改请求（`PrepareRequest` 语义）；`deny` 必须阻止工具执行。
- 审计 `hook.executed`（不进上下文）。

## 架构：PI 的扩展点阶梯

```
Run
 ├─ Context: base prompt + tenant sections + skills index（宿主装配）
 ├─ Hooks:  PI 扩展事件（before_agent_start / turn_* / tool_execution_* / context / compact）
 ├─ Tools:  builtin · skill tools · MCP tools（mcp__server__tool）
 ├─ Provider: StreamFn + 文件解析
 └─ Telemetry: TelemetryContext/Span/Metrics → adapters
```

**内核不内建业务语义；宿主只做装配；协议包只做边界。**

## 实施顺序与并行度

| 阶段 | 工作流 | 依赖 | 可并行 |
|---|---|---|---|
| P1 | 1 遥测契约 + adapters + `run.finished` | 无 | ✅ 与 2、3 并行 |
| P1 | 2 可恢复性（generation/tool 幂等、task、checkpoint） | 1 的 span 字段 | ✅ |
| P1 | 3 压缩（PI entry/checkpoint 语义） | 2 的 checkpoint | ✅ 后置 |
| P2 | 4 skills 宿主 loader + 权限 | 1 的 section 装配 | ✅ 与 6 并行 |
| P2 | 6 hooks（PI 事件名 + Go/命令两种） | 1 的事件点 | ✅ |
| P3 | 5 MCP 客户端（镜像 pi-mcp） | 4/6 的工具与权限 | ✅ 与 7 并行 |
| P3 | 7 provider 文件（keel-ga） | 无 | ✅ 独立 |
| P4 | 8 conformance + 混沌 + 2GB/1000/64 + 排水 | 全部 | ❌ 收口 |

## 验收

- 六项能力全部 G5；
- `docs/PI-PARITY.md` 的 conformance suite 全绿（事件回放、AgentTool、SKILL.md、MCP、压缩、遥测）；
- 10 分钟随机 kill：0 丢事件、0 重复最终回复；
- 2GB / 1000 会话 / 64 run：RSS < 1.8GB，0 OOM；
- 指标 series 不随会话数增长；日志无消息正文与密钥。

## Impact

- 新表：`tasks`（启用）、`skills`、`mcp_servers`、`hook_audit`、`checkpoints`（已有）。
- 新包：`internal/telemetry`、`internal/skills`（宿主层）、`internal/mcp`、`internal/hooks`。
- 新事件：`run.finished`、`hook.executed`、`compaction`（已有）。
- 兼容：现有 HTTP 契约不变；新增 `/metrics`、`/v1/ready`、`/compact`。
- 风险：MCP/Skills 引入外部代码与网络 → 必须有超时、权限与审计；不做「默认信任」。

## Open Decisions（需要 Owner 拍板）

1. **Skill 是否允许携带可执行脚本？** 建议 v1 只做提示词 + 工具授权；脚本留到沙箱之后。
2. **MCP v1 是否包含 stdio？** 建议包含（PI 默认支持），但要有租户级进程管理。
3. **Hook 形态？** 建议进程内 Go + 命令钩子都做（命令钩子兼容社区）。
4. **可观测是否上 OTLP？** 建议实现 PI telemetry 契约，Prometheus + 结构化日志为主，OTLP 默认关。
5. **GA 前是否抽 `pkg/` 稳定 SDK？** 建议抽最小公开面（事件、工具、provider、blob）。
6. **PI 兼容基线？** 已 pin `55a5c69`；升级策略见 `docs/PI-PARITY.md` §5。
