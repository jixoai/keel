# Proposal: keel-board-v1

> 立项：2026-11-27。参考标准：`vercel/board` @ `26428ed4e0661ff362c59abdf1215997c59891b0`
> （Apache-2.0）。已精读 `SPEC.md`、`packages/board/docs/01-board/*`、
> `packages/board/docs/02-sync/{01-overview,02-functions,03-slack,04-github,06-providers,07-linear}.md`、
> `packages/board/src/api/{protocol,routes}.ts`、
> `packages/board/src/inspect/app/{tasks,task,store}.ts(x)`、`packages/board/src/contract/{schemas,folds,external}.ts`
> 与 `packages/board/src/sync/{engine,provider,chat-provider}.ts`。
> 本 change 只转译**概念与可观察契约**，不复制其代码、测试或文档正文；不依赖 Vercel API、
> Vercel OIDC 或 `@vercel/board` 包。
> **交付路径（Owner 2026-11-27 追加）**：先在独立仓库
> `github.com/jixoai/keel-board`（Apache-2.0）完成完整标准（含 sync），keel 再以**固定
> commit 的 Go module 依赖**导入；keel 不先写一份平行实现，也不把 sync 降级为后续可选项。
> 上游 `vercel/board` commit pin：`26428ed4e0661ff362c59abdf1215997c59891b0`；
> keel-board 自身的 commit pin 在 phase 0 产出后写入 keel `go.mod`（pseudo-version），
> MUST NOT 依赖浮动 branch/tag。
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
任务没有依赖、状态没有统一标签、UI 没有统一队列，Slack/GitHub/Linear 的讨论也无法进入
同一工作日志。vercel/board 用一套很窄的契约证明了完整模型可以同时被 CLI、agent、人和
平台适配器使用：任务就是线程，回复就是工作日志，claim 就是带 epoch 围栏的租约，依赖与
标签就是队列路由，sync 让外部线程与 board 线程双向镜像。我们采用**这个标准**，但把它落到
keel 的 Account/Bot/RBAC/ReBAC、PostgreSQL、SSE 与 nsh 上，而不是照搬 Vercel 后端。

现在做的理由：身份（`keel-bot-identity-access`）、协作模式（`keel-collab-core` /
`keel-collab-superagent`）、会话投影（`keel-session-surfaces-v1`）与 web/TUI 客户端
（nsh 0.7.x）都已就位；缺的正是这一层通用任务/线程与同步契约。再晚一点，各插件会各自
长出一套不兼容的“任务卡”，Slack/GitHub/Linear 集成也会各自重复实现去重、重试和凭据隔离。

## 交付路径：独立仓库先行，keel 导入

| 阶段 | 仓库/依赖 | 内容 | 退出条件 |
| --- | --- | --- | --- |
| Phase 0 | `github.com/jixoai/keel-board`（独立、Apache-2.0） | 核心协议/日志/PG 存储/投影/API/SSE；任务/claim/needs/labels；watchers/schedules；**sync 引擎 + Slack/GitHub/Linear 原生适配器 + Chat SDK 通用桥**；完整 E2E | 独立仓库测试/E2E/复核全绿，产出不可变 commit 与 `v0.1.0` tag |
| Phase 1 | keel 依赖 `keel-board@<phase-0-commit>` | 身份/RBAC/ReBAC/PG/SSE/secret 适配；superagent 只读镜像→命令桥；启用 sync | keel 集成测试/E2E/复核全绿 |
| Phase 2 | keel + nsh | 队列/任务卡/线程/租约倒计时/live tail/五态；sync/watcher/schedule 状态与故障处理 UI | 全量 E2E、截图报告、复核、归档 |

强制约束：

- **keel-board 是标准的唯一实现源**。keel MUST NOT 同时维护第二套 `internal/board`
  领域实现；导入前不得以“临时实现”启动。
- keel 的 `go.mod` MUST 固定到 phase 0 产出的**不可变 commit**（`go get
  github.com/jixoai/keel-board@<sha>` 生成 pseudo-version），MUST NOT 跟随 branch 或可移动
  tag；升级 MUST 走独立 PR，并重跑 keel-board conformance + keel 集成测试。
- keel-board 的 `README`/`NOTICE` MUST 记录上游 `vercel/board` 的 commit 与 Apache-2.0
  归属；keel 的 `NOTICE` MUST 记录 `keel-board` 的归属与固定 commit。
- 独立仓库可以先于 keel 存在，但 MUST 在 phase 0 达到“无 keel 也能跑”的完整度：
  自带 in-memory/PG store、测试 auth/authorizer、假平台适配器与真实 webhook 签名测试。
- **sync 是 phase 0 的必须项，不是 phase 1/2 的增强**。没有 Slack/GitHub/Linear 双向镜像
  与 Chat SDK 通用桥，phase 0 不算完成，keel 不得导入。

## What Changes

1. **独立仓库 `keel-board`（phase 0）**：
   - 核心：channel/thread/message/label、任务七态、claim/lease/epoch/token、needs DAG、
     watch/schedule、attachments/reactions/mentions/blocks、SSE、审计/重建。
   - 存储：vendor-neutral `Store` 接口 + PostgreSQL 参考实现（`board_*` schema、索引、
     RLS、迁移）；in-memory store 供 conformance。
   - 协议：`/v1/board` 路由闭集 + webhook/同步管理/源读取路由；无状态、可重启。
   - sync：完整引擎（inbox/outbox、去重、重试、receipt、copy、thread mirroring）与
     Slack/GitHub/Linear 原生 Go 适配器；Chat SDK 通过**签名 HTTP bridge**接入任意
     Chat SDK adapter（不把 Node/Chat SDK 运行时嵌进 keel-board）。
2. **keel 导入（phase 1）**：以 Go module 固定 commit 导入；实现 keel 的
   Account/Bot 身份、`board.read/write/manage` + 频道 ReBAC、PostgreSQL/RLS、SSE hub、
   tenant secret store、指标/审计适配。
3. **superagent 适配（phase 1）**：Ticket→task 线程、Assignment→claim/lease、Attention→
   blocked(needs_input/awaiting_approval)、report→thread reply；先只读镜像，再命令桥；
   superagent 仍是其记录真值，不双写。
4. **UI（phase 2）**：nsh web/TUI 同一份 TSX；队列状态 tabs、channel/label/owner/for 过滤、
   oldest first；任务卡 claim/renew/release/complete/fail/block/unblock/reopen/assign/needs/
   cancel；线程回复；租约倒计时；live tail；sync 目标/收件箱/失败/重试状态；五态。
5. **sync 全量范围**（phase 0，详见下节）：Slack/GitHub/Linear 原生适配器 + Chat SDK
   通用桥；双向 thread 镜像、去重 marker、出站重试、凭据隔离、不可信输入、失败恢复。

**BREAKING**：无。所有表、路由、权限键与 UI 均为新增，默认由 tenant 级 feature flag 关闭；
不修改 Session 账本、superagent 已归档记录或既有 API 语义。

## Sync 全量范围（phase 0 必须项）

| 平台 | 入站 | 出站 | 关键能力 |
| --- | --- | --- | --- |
| Slack | `message.channels/groups`、edit/delete、reaction add/remove、files、link unfurl | root 起 thread、reply、edit/delete、reaction、files、typing | 名称/ID 频道解析、bot 身份、permalink、源读取、join |
| GitHub | PR/issue opened/closed/reopened、comments、review submitted/dismissed、workflow run→CI labels、label/assignee 同步 | 已有 PR/issue 线程的回复/编辑/删除/反应、状态标签重渲染 | review thread 归组、`state/review/tag.*/assignee.*` 标签、`botUserId` 回声抑制 |
| Linear | issue created/updated、comments、properties/labels/assignee | board root 创建 issue、回复/编辑/删除、状态/属性更新、反应（尽力） | team key 路由、状态类型 `triage/backlog/unstarted/started/completed/canceled`、源读取 |
| Chat SDK 通用 | 任意 adapter 归一化事件经签名 bridge 入站 | 通过 bridge 调用 adapter 的 post/edit/delete/react/typing/files | 不绑定具体平台；bridge 契约固定、可签名、可重放防护 |

共通语义：

- **双向 thread 镜像**：board root ↔ 外部 thread；回复/编辑/删除/反应/标签状态按规则双向；
  外部子线程（GitHub review thread、Linear comment thread）归组到主线程，回复按来源回到子线程。
- **去重 marker**：入站 `dedupe` = 平台 occurrence ID（≤300），`(tenant, key, dedupe)` 唯一；
  导入消息用确定性 client ID；出站 `delivery_id` = thread+target+message+revision；receipt/copy
  唯一；平台无可靠 bot 身份时，出站消息带隐藏 marker（HTML comment/平台 metadata），入站
  见 marker 即视为回声丢弃。
- **出站重试**：按 board thread lane 保序；失败 1s 起指数退避至 30s，5 次后记
  `message.sync_failed` 并继续后续消息；每 target 的失败可被后续成功清除。
- **凭据隔离**：platform secret 只经 per-tenant/per-entry `SecretResolver` 提供；不写 board
  日志/事件/指标；webhook 由 adapter 验签；bridge 用 HMAC+timestamp+nonce；最小 scope；
  可轮换、可停用，失败 fail closed。
- **不可信输入**：webhook body 限长、schema 校验、NUL/代理项/控制字符清洗、markdown/HTML
  消毒、URL scheme/host 白名单与私网拒绝、文件类型/大小限制、超时与无任意跳转。
- **失败恢复**：webhook 先落 inbox 再 ack；inbox lease/ack/退避/死信；重启从 lease 恢复；
  outbox durable；重复 webhook 幂等；单线程/单 target 失败不阻塞其他线程与 board 主路径。

## Capabilities

### New Capabilities

- `agent-board`：租户级任务/线程标准，含 board 事件日志与投影、频道/消息/标签、任务七态、
  claim 租约与 epoch/token 围栏、needs DAG、权限与租户隔离、SSE、审计/重建、watchers/
  schedules、attachments/reactions/mentions/blocks、**Slack/GitHub/Linear/Chat SDK 全量
  双向 sync**、nsh 队列与五态、superagent 适配边界。

### Modified Capabilities

无。本 change 不修改 `collaboration-superagent`、`task-log`、`session-surfaces` 的既有
Requirement。Phase 1 的 superagent 适配是**读取既有 Ticket/Assignment/Attention 并投影**，
superagent 仍是其记录的真值；若未来把 board 升为唯一真值，另立 change 并给
`collaboration-superagent` 写 MODIFIED delta。

## 与现有 superagent 工单看板的关系

| 维度 | superagent 工单看板 | `agent-board` |
| --- | --- | --- |
| 范围 | superagent 模式内部 | 租户级通用标准 |
| 工作单元 | Ticket + Assignment | task（线程根）+ claim（租约） |
| 人工等待 | Attention（decision/clarify/approval） | blocked（kind + reason） |
| 工作日志 | worker report / 结构化结果 | 线程回复 |
| 外部协作 | superagent 自己的 Source/Channel | 统一 sync：Slack/GitHub/Linear/Chat SDK |
| 真值 | `collab_*` 表 | `board_events` + 投影 |
| UI | 模式专用工单看板 | nsh 通用队列/任务卡/线程 |

superagent 的进入方式是**适配器**：phase 1 先单向镜像到 board（只读），再提供命令桥
（board 上的动作调用 supervisor 的既有 API），superagent 的 Mandate、Assignment、
Attention、审计与已归档语义全部保留。`agent-board` 不定义 Ticket/Mandate，也不接管
superagent 的派工策略；它只提供“任务/线程/认领/依赖/标签/实时队列/外部同步”这层通用语言。

## 归属与许可（Apache-2.0）

- 参考实现：`vercel/board`，commit `26428ed4e0661ff362c59abdf1215997c59891b0`，
  Apache License 2.0（见其 `LICENSE` 与 `NOTICE`）。
- 独立仓库：`github.com/jixoai/keel-board`，Apache License 2.0。其 `NOTICE` MUST 写明
  “独立实现，参考 vercel/board 的概念与契约；未复制源代码”，并保留上游项目名称与
  commit；若未来复制任何代码片段，MUST 保留版权头并更新 NOTICE。
- keel 以 module 依赖导入 keel-board；keel 的 `NOTICE` MUST 记录 keel-board 的
  `module path + 固定 commit + Apache-2.0`。MUST NOT 把 Vercel 名称/商标用于产品命名、
  域名或营销。
- keel 与 keel-board MUST NOT 依赖 `@vercel/board`、Vercel API、Vercel OIDC。
  Chat SDK 仅通过外部 bridge 协议接入，不把 Node/Chat SDK 运行时嵌入 keel-board/keel。

## Impact

- **新独立仓库**：`github.com/jixoai/keel-board`（Go module）：`board/`、`store/`、
  `server/`、`sync/`、`sync/slack|github|linear|chatsdk/`、`watch/`、`conformance/`、
  `cmd/keel-board/`；自带 LICENSE/NOTICE/README/CI。
- **keel 侧**：`go.mod` 固定 keel-board commit；`internal/boardintegration`（身份/RBAC/
  ReBAC/PG/SSE/secret/telemetry 适配）；`internal/superagent` 适配器；
  `web/src/screens/board/*` 与 `web/src/data/board.ts`。
- **存储**：keel-board 自带 `board_*` schema（core + sync + watch/schedule）；keel 复用同一
  PostgreSQL，按 tenant + RLS 迁移；不改既有表。
- **权限**：新增 `board.read` / `board.write` / `board.manage` 权限键；频道访问复用
  `resource_relations`（`resource_type=board_channel`，relation=owner/editor/viewer）；
  sync 配置/凭据/收件箱/源读取由 `board.manage` + 既有 secret 权限保护。
- **API/UI**：新增 `/v1/board/*`（含 sync webhook/管理/源读取）与 nsh 屏幕；不改既有
  `/v1/sessions/*`、`/v1/team-bots/*`。
- **运行时**：keel-board/keel 核心仍是 Go + PostgreSQL；Chat SDK bridge 是可选外部进程，
  由部署方按签名契约运行，不成为 keel 的运行时依赖。

## Out of Scope

- 照搬 Vercel 后端、Vercel API、OIDC、team/project/deployment 概念与 grants 引擎。
- 在 keel-board/keel 内嵌 Node/Chat SDK 运行时；只提供签名 bridge 契约。
- Slack/GitHub/Linear 之外的**原生**适配器；其他平台通过 Chat SDK bridge 接入。
- 平台账号/应用创建、OAuth 安装向导、Slack Connect/私有频道管理策略、DM/presence。
- GitHub 由 board root 自动创建 issue（与 vercel/board 标准一致；可通过后续扩展另立
  change；已有 PR/issue 线程的回复/编辑/删除是双向的）。
- 全文搜索、聚合/分组、跨租户 board、每线程 ACL、托管型公共看板。
- 重写 superagent 的 Ticket/Mandate/Assignment/Attention 语义，或迁移已归档工单。
- 不新增外部调度器/消息队列服务/语言运行时；watchers/schedules/sync 使用 PG durable 表 +
  进程内 worker（keel 导入后可复用 keel jobqueue/triggers）。
