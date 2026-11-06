# Tasks: keel-ecosystem

> 闸门定义见 `docs/MATURITY.md`。每项完成后在行尾标 `[G0]`…`[G5]` 的通过等级。

## 1. observability（P1，关键路径起点）

- [ ] 定义 `run.finished` 事件 schema（turns/tokens/toolTimings/context/model/provider/duration/stopReason）
- [ ] Runner 在 run 结束追加 `run.finished`（不进入模型上下文）
- [ ] provider usage 归一（OpenAI/Anthropic/Responses 字段差异）
- [ ] `/metrics` Prometheus 文本：runs/turns/tool/context/queue/lease/stream/pgxpool/go
- [ ] 指标基数审查：禁止 session/run id 标签；写进测试
- [ ] 结构化日志字段：request/tenant/session/run id；消息正文脱敏测试
- [ ] 可选 OTLP trace（默认关）：run span + turn/tool child spans
- [ ] 测试：一次 run 后可从指标/日志还原完整链路
- [ ] G3 证据：series 数在 1 与 1000 会话下相同

## 2. recoverability（P1，与 1 并行）

- [ ] `tasks` 表启用：kind/state/attempts/restart_limit/lease
- [ ] 模型生成任务：`generation_id` 写入 assistant 事件；恢复时幂等复用
- [ ] 工具任务：`(session_id, tool_call_id)` 唯一约束；重复执行只留一条结果
- [ ] 事件追加幂等：同 `(session, kind, generation_id/tool_call_id)` 不重复
- [ ] checkpoint 投影：`compaction` 事件 + `checkpoints`；恢复 = checkpoint + tail
- [ ] 崩溃矩阵五格：模型流 / 工具 / 追加 / 压缩 / SSE，每格一个测试
- [ ] 10 分钟随机 SIGKILL 混沌（nightly CI）
- [ ] G2 证据：0 丢事件、0 重复最终回复、run 状态收敛

## 3. compaction（P1，依赖 2）

- [ ] token 估算与阈值（复用 `internal/compaction`）
- [ ] 切点策略：保留最近 N token；不在 tool 中间切；不压缩最后一条 user
- [ ] 摘要请求走 provider；失败保留原上下文并记录
- [ ] 追加 `compaction` 事件 + checkpoint；`tokensBefore/After`
- [ ] 恢复时从 checkpoint + tail 重建
- [ ] 手动 `POST /v1/sessions/{id}/compact`
- [ ] 测试：超阈值压缩一次；历史事件仍可读；重复压缩不叠加

## 4. skills（P2，依赖 1 的 section 装配）

- [ ] `SKILL.md` 解析：frontmatter（name/description/allowed-tools/version/license）
- [ ] 目录扫描 + 租户注册表（`skills` 表，内容 hash 去重、版本不可变）
- [ ] 系统提示只放索引（name+description）；`skill` 工具按需加载正文
- [ ] `allowed-tools` 与实际工具集取交集；越权调用拒绝
- [ ] 脚本/资源：默认不执行；需显式权限（见 Open Decision 1）
- [ ] 热更新：文件变更下一轮生效；运行中 run 不受影响
- [ ] 兼容测试：真实 pi 格式 skill 零改动加载
- [ ] G4 证据：上游 SKILL.md 原样可用

## 5. hooks（P2，与 4 并行）

- [ ] 事件模型：turn.start/end、tool.before/after、run.stop、compaction.before/after
- [ ] 进程内 Go hook：可改 args、可 deny、可注入 guidance
- [ ] 命令钩子：JSON stdin/stdout；超时 5s；失败策略可配
- [ ] 决策语义：allow/deny/modify；deny 必须阻止工具执行
- [ ] 审计 `hook.executed` 事件（不进上下文）
- [ ] 测试：deny 生效、modify 生效、超时不挂 run、观察型失败不阻断
- [ ] G4 证据：一份社区风格 hook 配置零改动可用

## 6. mcp（P3，依赖 4/5 的工具与权限）

- [ ] MCP 客户端：stdio + streamable HTTP/SSE
- [ ] 工具发现/命名空间 `mcp__<server>__<tool>`；冲突检测
- [ ] 租户 `mcp_servers` 表：transport/command/url/env（加密）/allowed/timeout
- [ ] 连接池、健康检查、重连退避；server 崩溃不拖垮 run
- [ ] 凭据脱敏；破坏性工具默认需 hook/审批
- [ ] 兼容测试：真实 MCP server（filesystem 或 git）零改动接入
- [ ] G4 证据：真实 server 的 tools 可在 run 中调用

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
- [ ] MCP v1 是否包含 stdio（影响 6 的进程管理）
- [ ] Hook 是否两者都做（推荐是）
- [ ] 可观测是否上 OTLP（默认关）
- [ ] 是否抽 `pkg/` 稳定 SDK
- [ ] 兼容基线 pin 哪个上游 commit
