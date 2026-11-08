# Tasks: keel-agent

> 产品层（内核在 `keel-kernel-loop`）。A=AgentAPI/服务 · X=扩展面 · S=surface 核心 · U=NSH 客户端 · Q=验收 · R=复核。
> 纪律：内核包零 import 变化；子代理并行、worktree 内开发；NSH **pin main（nightly）**，不锁 release。

## A. `keel` CLI 与 Agent 服务

- [x] A1 `cmd/keel`：`serve`（Agent 服务）、`tui`（NSH TUI）、`version|doctor`；复用 servicehost 装配
- [x] A2 AgentAPI：`GET /v1/sessions`（列表/分页）、`POST /v1/sessions/{id}/cancel`（幂等）、
      `POST /v1/sessions/{id}/decisions/{decisionId}`、`GET /v1/sessions/{id}/events/stream?partials=1`、
      `GET /v1/capabilities`
- [x] A3 服务面：同源托管 `web/` 构建；认证中间件（cookie/网络中间件）；错误映射 401/409/429/503；
      SSE 原样透传（不引入 WebSocket）
- [x] A4 审批原语：decision 事件 + 等待/超时/取消 + 答案事件化（租户隔离、幂等）
- [x] A5 partial 旁路：进程内 hub 广播，非持久，不进事件表

## X. 扩展面（第一期）

- [x] X1 `internal/agentext`：插件契约（provides/requires/hooks/triggers/tools/effects）、
      装配与反序卸载；内置示例插件
- [x] X2 Sub-agent：`spawn_subagent` 原语（独立会话 + 结构化结果 + 预算）；父会话可见进度
- [x] X3 触发器：时间（cron/interval）与 WebHook；幂等键、审计、失败重试；触发→会话→turn
- [x] X4 Hooks 闭集：`subagent.started|finished`、`trigger.before|after`、`turn.started|finished`、
      `decision.opened|answered`；声明式订阅、不阻塞账本、失败可见
- [x] X5 扩展面 E2E：插件注册→触发→hook 执行→subagent 返回，全链路可观测

## S. chat-surface 核心（TS headless）

- [x] S1 事件→投影（Turn/workSegments/workLog/decision/queue）；AG-UI 映射；零 DOM
- [x] S2 queue/guide 状态机（与 zcode interaction-behavior 对齐）；stop barrier/reservation
- [x] S3 续传/幂等（after 游标、snapshot/delta、去重）
- [x] S4 审批/停止；S5 单测（fixtures 覆盖混排/崩溃/降级）

## U. NSH 客户端（web + TUI，同一份代码）

- [x] U0 NSH **pin main（nightly）**；本地检出 `~/Development/GitHub/longbridge/nsh` 可作参考；
      需要特性直接向作者提（例：`List alignment=bottom` 已落地，聊天列表必须使用）
- [x] U1 骨架/主题：`nsh.json` + solid-glass 令牌；侧栏+主区；`List alignment=bottom` 的聊天列表
- [x] U2 会话列表（新建/删除/搜索/空态/运行中标识）
- [x] U3 会话视图（消息/thinking/工具卡分组/图片/附件/压缩标记/run 状态/run.finished 摘要/流式）
- [x] U4 composer（queue/guide 切换 + 「等待引导当前任务…」+ 降级回队列 + 附件 + 停止）
- [x] U5 审批卡（decision/menu/options，一键作答/超时）
- [x] U6 TUI：`keel tui` 经 NSH TUI target 渲染（会话/对话/composer/审批）
- [x] U7 打包与截图：`nsh package --target web` 由 `keel serve` 托管；web/TUI 关键状态截图

## Q. 验收

- [x] Q1 AgentAPI E2E（列表/取消/审批/partials；取消与完成竞争）
- [x] Q2 扩展面 E2E（插件/subagent/trigger/hook）
- [ ] Q3 既有 11 门回归 + 内核纯度测试
- [ ] Q4 web/TUI 视觉走查（vision）+ HTML 图文报告（公网 local-* 链接）
- [x] Q5 文档：`docs/AGENT.md`（AgentAPI/扩展面/部署）、`docs/CHAT-SURFACE.md`、`docs/WEBUI.md`、`docs/KERNEL.md`

## R. 复核

- [ ] R1 子代理对抗复核（AgentAPI/扩展面/UI 状态机/内核纯度），阻塞修完再复审
- [ ] R2 全量验收、报告归档、change archive
