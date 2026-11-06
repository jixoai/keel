# Tasks: keel-ecosystem

> 闸门见 `docs/MATURITY.md`；PI 对齐基线见 `docs/PI-PARITY.md`（pin `55a5c69`）。
> 每项完成后在行尾标 `[G0]`…`[G5]` 的通过等级。

## 0. PI conformance 基座（贯穿）

- [ ] 录制/固化 PI 事件流 golden（`agent_start` → `turn_start` → `message_*` →
  `tool_execution_*` → `turn_end` → `agent_end`）
- [ ] `AgentTool` 形状对照测试（name/description/parameters/execute/executionMode）
- [ ] `CompactionEntry` / `CompactionCheckpoint` 语义对照
- [ ] `TelemetryContext/Span/Metrics` 形状对照
- [ ] CI 增加 conformance job（升级 PI pin 前必跑）

## 1. observability（P1，关键路径起点）

- [ ] 实现 PI telemetry 契约：`TelemetryContext` / `TelemetrySpan` / `TelemetryMetrics` /
  `TelemetryStore`
- [ ] `noop` / `memory` adapter（对齐 `packages/telemetry`）
- [ ] run = span；turn/tool/compaction = 子 span；`message_*` 作为 span event
- [ ] 业务汇总事件 `run.finished`（turns/tokens/toolTimings/context/model/duration/stopReason）
- [ ] provider usage 归一（OpenAI/Anthropic/Responses）
- [ ] Prometheus adapter：runs/turns/tool/context/queue/lease/stream/pgxpool/go
- [ ] 指标基数审查：禁止 session/run id 标签；测试证明 series 不随会话数增长
- [ ] 结构化日志字段 + 消息正文/密钥脱敏测试
- [ ] OTLP adapter（默认关）

## 2. recoverability（P1，与 1 并行）

- [ ] 对齐 PI durable：`GenerationTask` / `CompactionTask` / retry/progress policy
- [ ] 启用 `tasks` 表：kind/state/attempts/restart_limit/lease
- [ ] 模型生成：`generation_id` 写入 assistant 事件；恢复时幂等复用
- [ ] 工具结果：`(tenant_id, session_id, tool_call_id)` 唯一约束
- [ ] 事件追加幂等：同 `(session, kind, generation_id/tool_call_id)` 不重复
- [ ] checkpoint 回放：PI 语义的 compaction entry + checkpoint
- [ ] 崩溃矩阵五格：模型流 / 工具 / 追加 / 压缩 / SSE
- [ ] nightly 10 分钟随机 SIGKILL 混沌
- [ ] G2 证据：0 丢事件、0 重复最终回复、run 状态收敛

## 3. compaction（P1，依赖 2）

- [ ] 对齐 `DEFAULT_COMPACTION_POLICY`：阈值、保留量、切点
- [ ] 不在 tool 中间切；不压缩最后一条 user
- [ ] 摘要请求走 provider；失败保留原上下文并记录
- [ ] 追加 PI 语义的 `compaction` entry + checkpoint；`tokensBefore/After`
- [ ] 恢复从 checkpoint + tail 重建；历史仍可读
- [ ] 手动 `POST /v1/sessions/{id}/compact`
- [ ] 测试：超阈值压缩一次；重复压缩不叠加；回放一致

## 4. skills（P2，宿主层，内核不感知）

- [ ] Agent Skills 规范解析：`name`/`description`/`license`/`compatibility`/`metadata`/
  `allowed-tools`/`disable-model-invocation`
- [ ] 目录发现：`~/.agents/skills/`、`.agents/skills/`、项目/用户目录；递归找 `SKILL.md`
- [ ] 系统提示只放 name+description+path；`skill` 工具按需加载正文
- [ ] `/skill:name` 强制加载（PI 语义）；`enableSkillCommands` 等价配置
- [ ] 租户注册表：内容 hash 去重、版本不可变、同名冲突保留首个并告警
- [ ] `allowed-tools` 与实际工具集取交集；越权拒绝
- [ ] 脚本/资源默认不执行（见 Open Decision 1）
- [ ] 热更新：下一轮生效；运行中 run 不受影响
- [ ] G4：真实上游 SKILL.md 零改动加载

## 5. mcp（P3，独立包，镜像 `@earendil-works/pi-mcp`）

- [ ] `McpClient`：JSON-RPC 关联、初始化/能力协商、超时、取消、server 请求
- [ ] 传输：stdio、Streamable HTTP、PipeTransport、内存测试传输；**拒绝 legacy SSE**
- [ ] OAuth 子集（对齐 `pi-mcp/oauth`）
- [ ] 命名：`mcp__<server>__<tool>`，非法字符替换、冲突 hash、64 字符上限
- [ ] `normalizeMcpInputSchema`、`toLlmContent`、20KB 输出裁剪、progress 透传
- [ ] 租户 `mcp_servers`：`mcp.json` 兼容格式、env/headers 加密、日志脱敏
- [ ] 连接池/健康检查/重连退避；server 崩溃不拖垮 run
- [ ] 破坏性工具默认需 hook/审批
- [ ] G4：真实 MCP server（filesystem 或 git）零改动接入并在 run 中调用

## 6. hooks（P2，事件名对齐 PI）

- [ ] 事件：`before_agent_start`、`agent_start/end/before_settle/settled`、
  `turn_start/end`、`message_start/update/end`、
  `tool_execution_start/update/end`、`context`、`session_before_compact/compact/compact_failed`
- [ ] 进程内 Go：对齐 `defineExtension` / `hook` / `section` / `wrapTool` 语义
- [ ] 命令钩子：JSON stdin/stdout；超时 5s；失败策略可配
- [ ] 决策：`continue` / `deny` / 修改请求（`PrepareRequest`）
- [ ] 审计 `hook.executed`（不进上下文）
- [ ] 测试：deny 阻止工具、修改生效、超时不挂 run、观察型失败不阻断

## 7. provider 文件（P3，独立，见 keel-ga）

- [ ] 事件只存 blobId；provider 能力位与 resolver
- [ ] Gemini/OpenAI Responses/Anthropic adapter
- [ ] provider file 缓存 + TTL

## 8. 收口（P4，依赖全部）

- [ ] 2GB cgroup：1000 SSE + 64 run + 1MiB 图片，RSS < 1.8GB
- [ ] 10 分钟混沌报告
- [ ] `/v1/ready`、SIGTERM 排水、Dockerfile、compose、CI（nightly chaos）
- [ ] 公开 API 面评审（是否抽 `pkg/`）
- [ ] 文档：每个能力的 spec + 示例 + runbook
- [ ] 子代理交叉复核；HTML 报告 + 公网链接
- [ ] 归档本 change

## 待 Owner 决策（阻塞对应工作流）

- [ ] Skill 是否允许携带可执行脚本（影响 4 的权限模型）
- [ ] MCP v1 是否包含 stdio（影响 6 的进程管理）——PI 默认支持，建议包含
- [ ] Hook 是否两者都做（建议是）
- [ ] 可观测是否上 OTLP（默认关）
- [ ] 是否抽 `pkg/` 稳定 SDK
- [ ] PI pin 升级策略（当前 `55a5c69`）
