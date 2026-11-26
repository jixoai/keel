# Proposal: keel-board-v1

> 立项：2026-11-27。参考标准：`vercel/board` @ `26428ed4e0661ff362c59abdf1215997c59891b0`
> （Apache-2.0）。已精读 `SPEC.md`、
> `packages/board/docs/01-board/{04-messages,05-reading,06-watchers,09-access,11-tasks}.md`、
> `packages/board/src/api/{protocol,routes}.ts`、
> `packages/board/src/inspect/app/{tasks,task,store}.ts(x)` 与
> `packages/board/src/contract/{schemas,folds}.ts`。
> 本 change 只转译**概念与可观察契约**，不复制其代码、测试或文档正文；不依赖 Vercel API、
> Vercel OIDC、`@vercel/board` 包或 Chat SDK；不引入新语言/运行时。
> 基线：keel @ `8b35cb5`。定位：`agent-board` 是 keel 的**租户级通用任务/线程标准**；
> superagent 工单看板是它的一个来源与适配器，不是被替代的上位系统。

## Why

keel 已经有三套各自成立的协作事实，却没有一套跨模式的“任务队列”标准：

- **superagent 工单看板**（`collab_work_items` / `collab_assignments` /
  `collab_attentions`）只服务 superagent 模式：Ticket 有状态、Assignment 绑定 Worker
  Session、Attention 等待人工；但它不是通用任务面，普通 Bot、插件、人、外部 Issue 都进不来。
- **task-log**（`task_cards` / `task_events`）是**会话内**的任务卡投影：它回答“这次会话做了
  什么”，不回答“这个租户有哪些活、谁在干、谁可以接”。
- **Session 事件账本**是模型上下文的真值：它按会话串行，不适合作为跨会话、跨 Bot 的协作
  队列；把看板塞进某个会话账本会让权限、保留期和审计边界都失真。

结果是：同一个“待办 → 认领 → 阻塞 → 完成”的语义在每个入口重复实现，claim 没有租约、
任务没有依赖、状态没有统一标签、UI 没有统一队列。vercel/board 用一套很窄的契约证明了
这个模型可以同时被 CLI、agent、人和平台适配器使用：任务就是线程，回复就是工作日志，
claim 就是带 epoch 围栏的租约，依赖与标签就是队列路由。我们采用**这个标准**，但把它落到
keel 已有的 Account/Bot/RBAC/ReBAC、PostgreSQL、SSE 与 nsh 上，而不是照搬 Vercel 后端。

现在做的理由：身份（`keel-bot-identity-access`）、协作模式（`keel-collab-core` /
`keel-collab-superagent`）、会话投影（`keel-session-surfaces-v1`）与 web/TUI 客户端
（nsh 0.7.x）都已就位，缺的只是这一层通用任务/线程契约。再晚一点，各插件会各自长出一套
不兼容的“任务卡”。

## What Changes

1. **新增 `agent-board` 能力**：租户级 append-only board 事件日志
   （`board_events`，`tenant_id + seq`）与可重建投影（channels / messages / threads /
   tasks / labels / needs）。它不是 Session 账本，不进入模型上下文，不替代 task-log。
2. **频道、线程、消息、标签**：频道是路径（`work/production`，`work/` 表示子树）；
   线程只有一层；消息是 markdown 文本（≤10,000 字符）；标签是 `key=value` 过滤面；
   `board.*` 是保留命名空间，`board.task` 是任务状态标签。
3. **任务即线程根**：任务有 `waiting / ready / claimed / blocked / completed / failed /
   canceled` 七态；根文本是“要做什么”，回复是工作日志；owner 是创建任务的 Account，
   `by` 是实际执行创建动作的 Actor（Account 或 Account+Bot），`for` 可把任务留给 owner 的
   某个 Actor。
4. **Claim 是租约**：`epoch` 每次 claim 递增，`token` 只以 SHA-256 形式落库，写操作必须同时
   携带 `epoch + token`；租约默认 900 秒、范围 30–3600 秒，可 renew/release；过期后任务可被
   重新 claim，旧 claim 的任何写操作返回 `claim_lost`。
5. **依赖是 `needs` DAG**：最多 20 个前置任务；完成一个任务会在同一写事务里 ready 它的
   直接依赖者；失败/取消的前置不会 ready 任何任务；支持改 needs（无 live claim 且未完成时）；
   环、不可读、已取消、超深依赖一律 `invalid_needs`。
6. **Phase 1 API 最小闭集**：`/v1/board` 下的 channels、messages post/read、tasks
   create/list/claim-ready/claim/renew/release/complete/fail/block/unblock/reopen/assign/
   needs/cancel、任务/线程读、`GET /v1/board/events/stream`（SSE + `Last-Event-ID`）与
   `GET /v1/board/me`。权限为 `board.read` / `board.write` / `board.manage` 加频道级
   ReBAC 关系（viewer/editor/owner）。
7. **nsh 队列与任务卡**：web/TUI 同一份 TSX；队列按状态 tabs、channel/label/owner/for
   过滤、oldest first；任务卡提供 claim/renew/release/complete/fail/block/unblock/reopen/
   assign/needs/cancel；线程回复；租约倒计时；live tail；加载/空/错/权限不足/成功五态。
8. **Phase 2 适配**：superagent Ticket→task 线程、Assignment→claim/lease、Attention→
   blocked(needs_input)、report→thread reply；先单向镜像，再命令桥，不制造第二真值；
   GitHub 只做 issue/PR 的入站镜像与回复出站，复用既有 Source/Channel，不引入 Vercel sync。
9. **Phase 3 扩展**：watchers（durable tail 订阅与动作）、schedules（ durable 定时消息）、
   附件/反应/mentions/blocks（schema 校验的 JSON 块）、保留期与配额。

**BREAKING**：无。所有表、路由、权限键与 UI 均为新增，默认由 tenant 级 feature flag 关闭；
不修改 Session 账本、superagent 已归档记录或既有 API 语义。

## Capabilities

### New Capabilities

- `agent-board`：租户级任务/线程标准，含 board 事件日志与投影、频道/消息/标签、任务七态、
  claim 租约与 epoch/token 围栏、needs DAG、权限与租户隔离、SSE 事件流、审计/重建、
  nsh 队列与五态、superagent/GitHub 适配边界、watcher/schedule 扩展。

### Modified Capabilities

无。本 change 不修改 `collaboration-superagent`、`task-log`、`session-surfaces` 的既有
Requirement。Phase 2 的 superagent 适配是**读取既有 Ticket/Assignment/Attention 并投影**，
superagent 仍是其记录的真值；若未来把 board 升为唯一真值，另立 change 并给
`collaboration-superagent` 写 MODIFIED delta。

## 与现有 superagent 工单看板的关系

| 维度 | superagent 工单看板 | `agent-board` |
| --- | --- | --- |
| 范围 | superagent 模式内部 | 租户级通用标准 |
| 工作单元 | Ticket + Assignment | task（线程根）+ claim（租约） |
| 人工等待 | Attention（decision/clarify/approval） | blocked（kind + reason） |
| 工作日志 | worker report / 结构化结果 | 线程回复 |
| 真值 | `collab_*` 表 | `board_events` + 投影 |
| UI | 模式专用工单看板 | nsh 通用队列/任务卡/线程 |

superagent 的进入方式是**适配器**：phase 2 先单向镜像到 board（只读），再提供命令桥
（board 上的动作调用 supervisor 的既有 API），superagent 的 Mandate、Assignment、
Attention、审计与已归档语义全部保留。`agent-board` 不定义 Ticket/Mandate，也不接管
superagent 的派工策略；它只提供“任务/线程/认领/依赖/标签/实时队列”这层通用语言。

## 归属与许可（Apache-2.0）

- 参考实现：`vercel/board`，commit `26428ed4e0661ff362c59abdf1215997c59891b0`，
  Apache License 2.0（见其 `LICENSE` 与 `NOTICE`）。
- 本 change 转译的是**概念与可观察契约**：channel/thread/message/task/claim/lease/epoch/
  token/needs/labels/watcher/profile 等术语、状态机、错误语义、UX 队列模式。
- **不复制**其源代码、测试、样式、文案或文档正文。本 change 的 schema、Go 类型、路由
  形状与 UI 组件均按 keel 既有约定独立实现。
- 若后续实现需要直接借用其代码片段，MUST 先做许可证兼容审查，保留版权头，并在仓库
  `NOTICE` 中追加归属；MUST NOT 把 Vercel 名称/商标用于产品命名、域名或营销。
- keel 不引入 `@vercel/board`、Vercel API、Vercel OIDC 或 Chat SDK 依赖；认证沿用 keel 的
  Account 会话/API key。

## Impact

- **新增代码域**：`internal/board`（领域类型、状态机、fold、条件追加、权限），
  `internal/boardstore` 或 `internal/store/board_*.go`（PG 读写），`internal/httpapi/board_*.go`
  （路由、SSE、错误映射），`web/src/screens/board/*` 与 `web/src/data/board.ts`（nsh 同源）。
- **存储**：新增 `board_events`、`board_heads`、`board_requests`、`board_channels`、
  `board_messages`、`board_threads`、`board_tasks`、`board_task_needs`、`board_labels`
  （phase 2 再加 `board_links`；phase 3 再加 watchers/schedules）。全部 `tenant_id` +
  RLS；不改既有表。
- **权限**：新增 `board.read` / `board.write` / `board.manage` 权限键；频道访问复用
  `resource_relations`（`resource_type=board_channel`，relation=owner/editor/viewer），不新增
  第二套 ACL 表。
- **API/UI**：新增 `/v1/board/*` 与 nsh 屏幕；不改既有 `/v1/sessions/*`、`/v1/team-bots/*`。
- **依赖/运行时**：无新语言、无新运行时、无外部服务；继续 Go + PostgreSQL + nsh。
- **可观测**：新增 board 指标与审计事件；不把消息正文写进 `auth_events`。

## Out of Scope

- 照搬 Vercel 后端、Vercel API、OIDC、team/project/deployment 概念、grants 引擎与
  Chat SDK sync。
- 通用 Slack/GitHub 双向同步（phase 2 只做 GitHub issue/PR 的最小入站与回复出站）。
- 全文搜索、聚合/group-by、DM、presence、read receipts。
- 每个线程独立 ACL、跨租户 board、托管型公共看板。
- 重写 superagent 的 Ticket/Mandate/Assignment/Attention 语义，或迁移已归档工单。
- 附件、反应、mentions、schedules、watchers 的 phase 1 交付（分别在 phase 2/3）。
- 新的调度器/消息队列/语言运行时；watchers/schedules 复用既有 durable/jobqueue 与
  triggers 能力。
