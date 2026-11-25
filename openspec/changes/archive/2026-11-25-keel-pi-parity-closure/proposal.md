# Proposal: keel-pi-parity-closure

> 立项：2026-11-25。来源：`2026-11-25-keel-pi-parity-audit`（T1 已归档）。
> 目标是把审计确认的 PI 契约缺口接进产品，不恢复 `keel-ecosystem` 的旧任务板。

## Why

审计确认：68 条未勾里 44 条无需重写；真正缺的是 hooks 产品接线、遥测 span 树、
真实 MCP server E2E，以及若干指标/OTLP/chaos 加固。`internal/hooks` 库完整且有测试，
但不在 `cmd/keel` 依赖图，租户 `hooks.commands` 没有加载执行点；`docs/HOST.md` 的
超前声明已在 T1 校正。现在需要把库接进出货路径，并让 PI 的 span 形状可验证。

## What Changes

1. **PI hooks 产品接线**：租户 `hooks.commands` → `internal/hooks.Registry` → `agent.Runner`
   的 before_agent_start / context / tool_call / tool_result / session_before_compact；
   deny 阻止工具，modify 改参数/结果，超时不挂 run；审计记录不含正文/密钥。
2. **遥测 span 树**：run span 下挂 turn/tool/compaction 子 span，`message_*` 作为 span
   event；补 turns/tool/context 指标（固定基数）。
3. **OTLP adapter**：默认关；配置 `telemetry.adapter=otlp` 时使用 OTLP exporter，
   无 endpoint 不建连接。
4. **真实 MCP server E2E**：以 pinned `@modelcontextprotocol/server-filesystem` 走 stdio，
   零代码改动读取临时文件；工具面仍为恒定 11 个聚合工具。

## Impact

- 代码：`internal/hooks` 接入 `internal/agent`/`internal/servicehost`/`internal/loop`；
  telemetry/metrics/OTLP；E2E 脚本与 evidence。
- 不改 `keel-ecosystem/tasks.md`；旧 6 份 spec 继续冻结。
- 不新增客户端可见事件 kind；hook 审计先走结构化日志，避免改变 R1 事件流。

## Out of scope

- per-tenant MCP manager、连接池/健康检查、模型路径 MCP 破坏性审批、手动 compact 路由、
  `/skill:name`、文件监听式 skill 热更新（P2，见 audit.md）。
