# Proposal: keel-ecosystem

> 立项：Keel 首版与 Agent 执行落地后（2026-11-06）。本 change 把 **pi 的标准与主要生态**
> 接进 Keel：可观测、可恢复、上下文压缩、Skills、MCP、Hooks。
> **验收标准不是「功能有了」，而是 `docs/MATURITY.md` 的 G0–G5。**

## Why

Keel 已经有：无状态事件日志、SSE 续传、流式 blob、反压、持久 run、工具执行与崩溃恢复。
但一个「能上线」的 Agent 底座还缺三层东西：

1. **自证**：没有遥测就不知道一次 run 花了多少 token、卡在哪个工具、上下文多满；
2. **自愈**：模型流/工具执行之外还有追加、压缩、SSE 等 await 点，崩溃矩阵未闭合；
3. **生态**：pi/社区已有 SKILL.md、MCP、hooks 的成熟约定，Keel 不能自造一套。

社区实现很多，但大多是单机、有状态、绑定某个宿主。Keel 的差异是**无状态 + 多租户 + 2GB**，
所以这些能力**大部分要按 Keel 的契约重写**，只对齐行为与产物格式，不复制实现。

## What Changes

### 1. `observability`（先做）

- 每次 run 追加一条 `run.finished` 事件（不进入模型上下文）：
  `{turns, tokensIn, tokensOut, toolTimings, contextTokens, model, provider, durationMs, stopReason}`。
- Prometheus 文本指标：runs/turns/tool 时长/上下文 token/队列/租约/流/PG/Go runtime。
- 结构化日志：`request_id`、`tenant_id`、`session_id`、`run_id`；**不记录消息正文**。
- 可选 OTLP trace：run = 一个 span，turn/tool = 子 span；默认关闭。
- **G3 的硬约束**：指标 series 数不随会话数增长；禁止 session/run id 作为标签。

### 2. `recoverability`（与 1 并行）

- `tasks` 表启用：模型生成与工具执行都是持久任务，状态机 + attempts + restartLimit。
- **generation 幂等**：每次模型请求带 `generation_id`；恢复时若该 id 的 assistant 事件已存在，
  直接复用，不再请求模型——解决「模型流 kill 后重试导致重复回复」。
- **tool 结果幂等**：`(session_id, tool_call_id)` 唯一约束，重复执行只允许一条结果事件。
- checkpoint 回放：`compaction` 事件 + `checkpoints` 表；恢复 = 最近 checkpoint + tail。
- 崩溃矩阵：模型流、工具执行、事件追加、压缩、SSE 发送五格，每格一个 kill 测试。
- 10 分钟随机 kill 混沌测试进 CI（nightly）。

### 3. `compaction`（依赖 2 的 checkpoint）

- token 估算 → 超阈值 → 选切点（保留最近 N token）→ provider 摘要 → 追加 `compaction`
  事件 + 写 checkpoint → 用摘要 + 保留段重建上下文。
- 手动 `POST /v1/sessions/{id}/compact`。
- 约束：不在工具调用中间压缩；不压缩最后一条 user；摘要失败保留原上下文并记录错误。
- 历史事件永远可读；压缩只是投影。

### 4. `skills`

- 加载 pi 格式 `SKILL.md`：frontmatter（name/description/allowed-tools/version/license）+
  正文 + 可选脚本/资源。
- **渐进披露**：系统提示只放 name+description；正文由 `skill` 工具按需加载。
- 租户级注册表：文件系统目录 + `skills` 表（内容 hash 去重、版本不可变）。
- 权限：`allowed-tools` 与实际工具集取交集；skill 脚本不得未经许可执行；加载失败给出可读错误。
- 热更新：文件变更后下一轮生效；运行中的 run 不受影响。

### 5. `mcp`

- MCP 客户端：stdio（本地）与 streamable HTTP/SSE（远程）两种传输。
- 工具发现与命名空间：`mcp__<server>__<tool>`；冲突检测；超时与重连退避。
- 租户级 `mcp_servers` 表：name/transport/command/url/env（加密）/allowed tools/timeout。
- 资源与 prompts 先做只读发现，v1 不做双向采样。
- 安全：凭据脱敏；破坏性工具默认需 hook/审批；server 崩溃不拖垮 run。

### 6. `hooks`

- 事件：`turn.start`、`turn.end`、`tool.before`、`tool.after`、`run.stop`、`compaction.before/after`。
- 两种实现：
  - **进程内**：Go 接口（可改 args、可拒绝、可注入 guidance）；
  - **命令钩子**：JSON stdin/stdout，兼容社区习惯（超时、非阻塞、失败策略可配）。
- 决策语义：`allow | deny | modify`；`deny` 必须让工具不执行并留下审计事件。
- 审计：`hook.executed` 事件（不进入上下文），记录 hook 名、耗时、决策。
- 超时默认 5s；观察型 hook 失败只记日志，不阻断 run。

## 架构：扩展点阶梯

沿用「配置 → 技能 → 插件 → hook → 装配 → 展示层」的阶梯，**内核不内建业务语义**：

```
Run
 ├─ Context assembly: base prompt + tenant sections + skills index
 ├─ Hooks:  before/after turn · before/after tool · stop · compaction
 ├─ Tools:  builtin · skill tools · MCP tools (namespaced)
 ├─ Provider: model stream + image/file resolver
 └─ Telemetry: run/turn/tool events → log · Prometheus · (OTLP)
```

每个扩展点都要求三件套：**稳定 Go 接口 + 声明式配置 schema + 版本号**。
外部产物（SKILL.md / hook JSON / MCP server）只经适配器进入，不渗透内核。

## 实施顺序与并行度

| 阶段 | 工作流 | 依赖 | 可并行 |
|---|---|---|---|
| P1 | 1 可观测契约 + `run.finished` + `/metrics` | 无 | ✅ 与 2、3 并行 |
| P1 | 2 可恢复性（generation/tool 幂等、task、checkpoint） | 1 的事件字段 | ✅ |
| P1 | 3 压缩接线 | 2 的 checkpoint | ✅ 后置 |
| P2 | 4 skills 加载器 + 权限 | 1 的 section 装配 | ✅ 与 5 并行 |
| P2 | 6 hooks 内核 + 命令钩子 | 1 的事件点 | ✅ |
| P3 | 5 MCP 客户端 | 4/6 的工具与权限模型 | ✅ 与 7 并行 |
| P3 | 7 provider 文件 adapter（keel-ga） | 无 | ✅ 独立 |
| P4 | 8 混沌 + 2GB/1000 + 64 run + 排水 | 全部 | ❌ 收口 |

**关键路径**：1 → 2 → 3 → 8；其余可并行。接口在 P1 结束前冻结，避免生态适配返工。

## 验收（详见 docs/MATURITY.md）

- 六项能力全部 G5；
- 一个真实 pi skill、一个真实 MCP server、一份 hook 配置**零代码改动**可用；
- 10 分钟随机 kill：0 丢事件、0 重复最终回复；
- 2GB / 1000 会话 / 64 run：RSS < 1.8GB，0 OOM；
- 指标 series 不随会话数增长；日志无消息正文与密钥。

## Impact

- 新表：`tasks`（启用）、`skills`、`mcp_servers`、`hook_audit`、`checkpoints`（已有）。
- 新包：`internal/telemetry`、`internal/skills`、`internal/mcp`、`internal/hooks`。
- 新事件：`run.finished`、`hook.executed`、`compaction`（已有）。
- 兼容：现有 HTTP 契约不变；新增 `/metrics`、`/v1/ready`、`/compact`。
- 风险：MCP/Skills 引入外部代码与网络 → 必须有超时、权限与审计；不做「默认信任」。

## Open Decisions（需要 Owner 拍板）

1. **Skill 执行模型**：只做提示词+工具授权，还是允许携带可执行脚本（需要沙箱）？
2. **MCP 传输范围**：v1 是否包含 stdio（本机进程）？多租户下谁来管进程生命周期？
3. **Hook 形态**：进程内 Go 扩展、命令钩子，还是两者都做（推荐两者，命令钩子兼容社区）？
4. **可观测后端**：只做 Prometheus + 结构化日志，还是同时做 OTLP trace（默认关）？
5. **公开 API**：当前代码全在 `internal/`；GA 前是否抽 `pkg/` 作为稳定 SDK（推荐抽最小面）？
6. **兼容基线**：以 pi 的哪一版 SKILL.md / hook 格式为准（需要 pin 一个上游 commit）。
