# 宿主层（Host）设计

> PI 的三层里，**内核**（loop/durable）和**协议**（MCP/telemetry）已分别复刻；
> 本文件定义把它们装配成「能跑一个真实 Agent」的**宿主层**。只做装配，不重写内核语义。

## 1. 职责

宿主层把「租户配置 + 事件日志 + 工具 + 技能 + 钩子 + 协议」组装成一次 run：

```
Tenant config
  ├─ system prompt sections (base + skills index + tenant)
  ├─ tools      (builtin + skill loader + MCP namespaced)
  ├─ hooks      (PI extension event names)
  ├─ compaction (PI policy + checkpoint callback)
  └─ telemetry  (PI TelemetryContext/Span)
        │
        ▼
  loop.RunConfig  →  agent.Runner  →  events (PG)  →  SSE
```

**内核不感知** skills/MCP/租户；宿主只做装配与配置解析。

## 2. 配置 schema（v1）

```yaml
agent:
  name: "tenant-agent"
  provider: "openai"        # 空 = fake provider
  model: "gpt-5"
  systemPrompt: "You are ..."
skills:
  enabled: true
  roots: ["~/.agents/skills", ".agents/skills"]
mcp:
  servers:
    filesystem:
      command: "npx"
      args: ["-y", "@modelcontextprotocol/server-filesystem", "."]
      timeout: 60
hooks:
  commands:
    - event: tool_execution_start
      command: ["./hooks/deny-rm.sh"]
      timeoutMs: 5000
compaction:
  enabled: true
  threshold: 0.8
  keepRecentTokens: 20000
telemetry:
  adapter: "memory"         # memory | prometheus | otlp（后两者后续）
tools:
  allow: ["clock", "echo", "skill", "mcp__*"]
```

配置来源：**当前实现**从环境变量读取（见 §4）；`tenants.config jsonb` 是下一阶段的多租户配置面。
密钥只从 env/secret store 读，不落 schema。

## 3. 装配顺序（与 PI 对齐）

1. **Skills**：`skills.Discover(roots)` → `skills.FormatForPrompt(skills, "read")`；
   作为 system prompt 的 `<skills>` section；`disable-model-invocation` 不进入索引。
2. **Tools**：内置工具 + `skill` 工具（按需加载正文）+ MCP 工具（`mcp__server__tool`）。
   工具集按 `tools.allow` 过滤；skill 的 `allowed-tools` 与实际集合取交集。
3. **Hooks**：把 `internal/hooks` 的事件接到 loop 的 `PrepareRequest` / `OnStop` /
   `ShouldStopAfterTurn` / tool executor 的 before/after；命令钩子按配置加载。
4. **Compaction**：`loop.LoopConfig.Compaction` 使用 PI `DEFAULT_COMPACTION_POLICY`
   （threshold/keepRecentTokens/backgroundTokens）；`OnCompaction` 追加 `compaction` 事件 +
   写 `checkpoints`（投影，不覆盖事件）。
5. **Telemetry**：run = span，turn/tool = 子 span；`run.finished` 作为业务汇总事件追加。
6. **System prompt**：按 PI 的 section 形态拼装（`<preamble>`、`<skills>`、`<tenant>`），
   不再用单一字符串硬编码。

## 4. 与现有运行时的接线

- `internal/agent.Runner` 通过 `Assembly` 接缝（`SystemPrompt()`, `Tools()`）使用宿主；
  默认实现是 `internal/host.Host`，`Runner` 仍只负责事件↔消息、run 状态、租约。
- `/v1/sessions/{id}/input` 提交后入队 run；worker 领取后由 Runner 用装配好的 prompt/tools
  调用 loop。`Runner.Telemetry` 为 run 级 span + `keel.agent.runs` / `keel.agent.run.duration`。
- MCP server 生命周期由 `mcp.Manager` 管理（并发连接/列工具/部分失败报告/关闭）；
  run 结束不关闭共享连接；`cmd/runtime` 在退出时 `Close()`。
- 配置热更新：下一轮 run 生效；运行中的 run 用快照。

### 当前环境变量（`cmd/runtime`）

| 变量 | 作用 |
|---|---|
| `KEEL_SYSTEM_PROMPT` | preamble（默认一句通用身份说明） |
| `KEEL_SYSTEM_SECTIONS` | 额外 section，JSON 数组 `[{"name":"tenant","content":"..."}]` |
| `KEEL_SKILLS_ROOTS` | 技能根目录，OS path list（如 `~/.agents/skills:/srv/skills`） |
| `KEEL_FILE_READ_TOOL` | skills 索引里的读文件工具名：`read`（默认）/`bash`/`indirect` |
| `KEEL_MCP_CONFIG` | 全局 `mcp.json` 路径（空 = 不启用 MCP） |
| `KEEL_MCP_PROJECT` | 项目 `mcp.json` 路径（仅可信项目） |
| `KEEL_TELEMETRY` | `memory` 时启用内存 telemetry recorder |
| `KEEL_MODEL` / `KEEL_BASE_URL` / `KEEL_PROTOCOL` / `KEEL_PROVIDER` / `KEEL_API_KEY` | provider 选择（空 = fake provider） |

## 5. 验收

- 一个租户配置：1 个 skill + 1 个 MCP filesystem server + 1 个 deny hook + compaction +
  telemetry memory adapter；
- 一次对话：skill 索引出现在 system prompt、MCP 工具可调用、deny hook 阻止危险工具、
  超阈值触发 compaction、run span 有 turn/tool 子 span；
- `go test -race ./internal/host` + 运行时 E2E 全绿。

## 6. 非目标

- 不复刻 coding-agent 的 TUI/CLI/主题/键位/npm 包管理；
- 不复刻 project-trust UI 与交互式 `/mcp` 菜单；
- 不在宿主层实现新的协议或事件名——只装配 PI 已有语义。
