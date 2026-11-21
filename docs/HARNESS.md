# HARNESS — `keel serve` 产品层

`keel` 是内核（`docs/KERNEL.md`）之上的**第一个验证者**：一个 Harness 服务 + 一套产品 API +
扩展面（插件/触发器/subagent/hooks/审批），供 AI UI（`web/` 的 NSH 客户端、TUI）对接。
内核不知道它存在；依赖方向只有 `cmd/keel → agentapi/agentext → kernel`。

> 命名：产品面统一 **Bot / TeamBot / Account / Role / Profile**；`Harness` 是 Bot 的运行环境
> （工具白名单 + 上下文）。旧称 `Agent`/`TeamAgent`（已删除），只有 PI 技术包
> （`agentcore`/`agentloop`/`agenttool`/`internal/agent`）与协议名 `AgentAPI`/`AgentBackend`
> 作为技术名词保留。术语与别名说明见官网术语表（`site/docs/glossary.md`）。

## 1. 启动

```bash
# 本地（口令认证 + 同源托管 web/dist）
KEEL_SERVE_TENANT=<tenant-uuid> keel serve \
  --dsn 'postgres://…' --addr 127.0.0.1:13750 \
  --password <passphrase> [--model … --base-url … --api-key …]

keel tui                 # NSH TUI（同一份 App.tsx，见 docs/WEBUI.md）
keel doctor              # 只读诊断：配置/DB/schema/web 构建/TUI 产物
keel version
```

- 认证是**网络中间件**（`cmd/keel/auth.go`）：HMAC 签名 HttpOnly cookie，Bearer 兼容 CLI/TUI；
  服务端注入租户、剥离客户端 `X-Tenant-ID`。`--password ""` 关闭认证（仅回环开发）。
- 静态 UI 同源托管 `web/dist`（`nsh package --target web` 的产物；未构建时 JSON 404 带构建指引）。
- `--password ""` 关闭认证时仍注入配置的 tenant（自带 UI 可用），且只回答回环 Host（阻断 DNS rebinding）。
- `keel serve` 是**单租户**产品进程（`KEEL_SERVE_TENANT`）：扩展面读接口（plugins/triggers）返回该进程的
  全局装配状态，多租户复用需另立 change（按租户过滤 + 装配隔离）。
- 装配面是 `cmd/runtime` 的子集（kernel/worker/LLM/MCP/插件/触发器）；外部 AgentBackend、telemetry adapter、
  MCP OAuth、技能/压缩等仍只在 `cmd/runtime` 装配。
- 端口一律 13610–13799（`--addr` 默认 `127.0.0.1:13750`）。

## 2. API 面

### 核心 shell（`internal/httpapi`，内核直连）

| 方法 | 路径 | 语义 |
| --- | --- | --- |
| POST | `/v1/tenants` | 建租户（admin key） |
| POST | `/v1/sessions` | 建会话 |
| POST | `/v1/sessions/{id}/input` | 提交输入；`{text, requestId?, delivery?: queue\|guide}` |
| GET | `/v1/sessions/{id}/events?after=&limit=` | 已提交事件（快照/续传） |
| GET | `/v1/sessions/{id}/events/stream?after=&partials=1` | SSE；`id:` = 事件 seq，partial 无 id |
| GET | `/v1/sessions/{id}/run` | run 行（status/attempts/cursorSeq/lastError） |
| POST | `/v1/runtime/drain`、`GET /v1/runtime/status` | 排空/交接 |

### 产品 API（`internal/agentapi`）

| 方法 | 路径 | 语义 |
| --- | --- | --- |
| GET | `/v1/capabilities` | 能力集 + 闭集 hooks + 分页上限（前端能力门的数据源） |
| GET | `/v1/sessions?limit=&after=` | 会话列表（keyset 分页，`runStatus` overlay） |
| GET | `/v1/sessions/{id}/team-tasks?limit=&after=` | captain 会话的成员任务卡只读投影（`parentTaskId`，租户/会话边界，keyset 分页） |
| POST | `/v1/sessions/{id}/cancel` | 停止当前 attempt（幂等；202=cancelling，501=跨进程无 seam） |
| GET/POST | `/v1/sessions/{id}/decisions` | 审批开/列；`POST .../{decisionId}` 作答（幂等，409 冲突） |
| GET | `/v1/plugins` | 插件/generation/hooks/工具 |
| GET | `/v1/triggers`、POST `/v1/triggers/{id}/fire` | 触发器审计与手动/签名触发（幂等键；签名头见 §3.3）；fire 以 trigger 的 `botId` 创建 Bot-owned Session，`created_by` = 调用者 |
| GET | `/v1/subagents`、`/v1/subagents/{id}` | 子代理列表/详情 |
| POST | `/v1/sessions/{id}/subagents` | 派生子代理（`wait:true` 同步等待；断线不再取消子代理） |

错误映射：401 未认证 / 404 不存在 / 409 冲突 / 429 限流 / 501 seam 未接线 / 502 子代理或后端失败 / 503 排空中。

取消的语义：kernel 的 run 行按失败终态结算（attempt 不自动重跑），产品的 `runStatus` overlay 记 `cancelled`；重复 cancel 在同一进程生命周期内幂等（重启后按 durable 终态回答）。

## 3. 扩展面（`internal/agentext`）

| 能力 | 契约 |
| --- | --- |
| 插件 | **进程内声明式契约**（`agentext.Plugin`，`internal/agentext/plugin.go:67`）：`ID/Version/Provides/Requires/Tools/Hooks/Triggers/Effects/Start/Stop`；依赖拓扑激活（缺依赖 `PENDING` 可见）、反序卸载（hook 订阅/触发器/effects 精确撤销，无残留）、`generation+1`、工具重名 fail-closed（第二个插件 `FAILED`）；装配结果经唯一 sink 发布给模型工具集（`cmd/keel/serve.go` 用 `rt.RefreshProductTools`）。web 设置 › 插件（`web/src/screens/settings.tsx:1148`）渲染该装配面。**规划中**（`keel-plugin-core`，勿当作已实现）：`plugin.yaml` 包与安装/启用/禁用 API、三种 kind（in-process/sidecar/webview）、运维权限与审计、webview UI 托管与 `keel.plugin.*` 桥、创造模式 |
| GET | `/v1/triggers`、POST `/v1/triggers/{id}/fire` | 触发器审计与手动/签名触发（幂等键；签名头见 §3.3）；fire 以 trigger 的 `botId` 创建 Bot-owned Session，`created_by` = 调用者 |
| Sub-agent | `agent` 聚合工具的 `spawn` action：独立子会话 + 结构化结果 `{status,summary,artifacts,usage}` + 预算（turns/tokens/timeout）；父会话经 `parentSessionId` 可查 |
| Hooks | 闭集：`subagent.started\|finished`、`trigger.before\|after`、`turn.started\|finished`、`decision.opened\|answered`；有界异步队列，不阻塞账本，失败可见 |
| 审批 | decision 原语：open/wait/timeout/cancel + 答案 journal；durable 在 tasks 行 |
| Partials | token 级旁路：进程内 hub → SSE `event: partial`（无 `id:`，不进事件表，不重放） |

### 3.1 触发器交付语义（W10）

- **HMAC（webhook）**。`TriggerSpec.WebhookSecret` 是每个触发器的 HMAC-SHA256 密钥
  （至少 16 字节），`json:"-"`，绝不进入 `GET /v1/triggers`、插件快照或日志；生产用
  `WebhookSecretEnv` 指向环境变量（内置示例：`KEEL_EXAMPLE_WEBHOOK_SECRET`），注册时解析一次。
  签名覆盖**原始 body 字节**，canonical 为：

  ```
  v1\n<tenantId>\n<triggerId>\n<timestamp>\n<idempotencyKey>\n<rawBody>
  ```

  请求头：`X-Keel-Timestamp: <unix 秒>`、`X-Keel-Signature: sha256=<hex>`；
  另有 `X-Keel-Webhook: 1`（可省略，出现签名头即按 webhook 处理）。
  时间戳必须在 `webhookToleranceSec`（默认 300s，上限 86400s）内，签名用
  `hmac.Equal` 常量时间比较。
- **fail-closed**。未配置 secret 的 webhook 触发拒绝一切签名投递（403）；缺失/错误/过期签名
  返回 401。两者都写审计 `status:"rejected"`，错误只带类别（`missing_timestamp` /
  `stale_timestamp` / `bad_signature` 等），不带 secret 或收到的签名。手动 fire 是已认证的
  运维动作，不需要签名。
- **去重**。幂等键仍是唯一身份：同键并发按 key 串行化，进程内重复投递返回
  `deduped:true` 并审计 `status:"deduped"`；跨重启由 kernel 的 requestId 去重，返回
  `fired + deduped:true`。重复投递不会产生第二条 `user.message`。
- **漏跑补偿**。`trigger_state(tenant_id, trigger_id, last_scheduled_at, ...)` 持久化
  “已处理的最新计划窗口”。重启/注册时按该游标计算 `(last, now]` 内错过的窗口：
  只补 `catchUpLimit`（默认 3）个**最近的**窗口，且不早于 `catchUpWindow`（默认 24h）；
  更老的窗口合并跳过，审计 `status:"skipped"` 并记录 coalesced 数量。补跑使用与正常计划相同
  的稳定幂等键 `sched:<id>:<unix>`，所以即使补偿重复也不会二次提交。停机会话恢复不会
  产生风暴；上限内仍失败的窗口由审计可见。
- **并发上限**。`maxConcurrent`（默认 1）限制同一触发器同时在飞的 fire。超限返回
  `ErrTriggerBusy` → HTTP 429 + `Retry-After: 1`，审计 `status:"throttled"`；不排队，
  因为无界队列会把过载隐藏成延迟。同幂等键的重试先等待该键的首次 fire，再走去重分支，
  不会被当作超限。
- **初始任务卡**。一次成功 fire 在提交 `user.message` 后立即调用
  `tasklog.EnsureCard`，返回 `taskCardId` 并写审计；失败不回滚已提交的回合，而是返回
  `taskCardError`/审计字段（首次工具调用仍会懒创建）。满足 task-log「会话起步即有卡」。
- **观测**。`GET /v1/triggers` 的每个条目带 `fires/failures/dedupes/throttled/rejected/skipped`、
  `inFlight`、`webhookConfigured`、`lastScheduledAt` 与最近 20 条审计（fired/failed/deduped/
  throttled/rejected/skipped）。
### 3.2 工具面授权（A4.2b1）

模型能看到的每一个工具调用都在执行前经过同一套 access 授权，HTTP 面的授权不是工具面的替代品：

- **主体**：从 Session 归属读取——Bot 所属 Account、Bot handle 与 Bot Harness 的
  `context.permissions`；有效权限 = Account grants ∩ Harness permissions。Role/Instruction
  不产生授权。没有可解析主体（无主 Session、禁用账号、归档 Bot）一律拒绝。
- **key**：每个工具/action 在代码内唯一 registry 声明 required key（`internal/toolsurface`）。
  read/write/edit → `file.*`；bash → `shell.exec`；tasks → `task.read|write`；
  mcp → `mcp.use`；skills 读 → `skill.read`、库写 → `skill.manage`；agent → 目标 Bot 的
  `bot.invoke`；approve → `approval.read|write`；notify → `notification.read|write`；
  creation → `plugin.invoke`；媒体 → `media.invoke`；decision → `decision.ask`。
  插件发布的工具按 `plugin.invoke` 授权。未知工具/未声明 action/未登记 key 是装配错误。
- **资源**：Session 级工具以当前 Session 为资源；`agent` 的 spawn/fork/resume 以目标
  Bot（顶层 `botId`、`selection.botId`、子 Session 所属 Bot 的并集；未命名时为本
  Session 的 Bot）为资源，每个目标都要 `bot.invoke` 并各自写 `bot.invoked`；带
  `sessionId` 的参数以该 Session 为资源（跨 Session 通知读取另需 `session.read`）。
  文件目前没有 per-file 关系元组（见 tasks A4.2b 登记），以所属 Session 为资源边界。
- **captain → member**：父级必须对目标持有 `bot.invoke`；撤销后下一次调用即 403。HTTP 的
  `botId`/`selection` 子代理路由走同一检查，旧路径不是旁路。子 Session 继承父 Bot 归属，
  但每次调用按**自己**的 Account ∩ Harness 判定，不做调用链交集。
- **失败语义**：无 access seam、无 principal、授权源读取失败、invoke 审计写失败均 fail
  closed；工具错误只告诉模型缺哪个 key/reason，不透露资源是否存在。
- **审计**：拒绝写 `permission.denied`（含 key、reason、资源类型）；接受的 `bot.invoke`
  写 `bot.invoked`（调用者、被调用 Bot、Session、结果）。

### 3.2 Session 执行绑定（A4.2b3a）

- **归属**：`sessions.bot_id` / `team_bot_id` 是归属事实；`account_id` 由 Bot 的 Profile
  派生，`created_by_account_id` 记录发起人。Session 创建后归属不可被输入改变；旧 `bound_*`
  投影列已删除。无 Bot 归属的旧 Session 不进入运行（fail closed）。
- **选择事实**：输入的 `selection` 只接受 `botId` / `teamBotId`；旧 `agentId`/`teamId`
  是未知 key（400），不静默回退。未带选择时按 Session 归属的 Bot 解析；带其他 Bot 的选择
  只改变该尝试的 prompt/tools（委派子 Session 保留父 Bot 归属）。
- **Prompt**：`Role.Instruction` + `Harness.Context.Instruction`（去重、Role 在前）。
- **工具**：`Harness.Tools` 是白名单；实际工具面 = 部署 registry ∩ 白名单，空列表不授予
  任何工具（不是“继承全部”）。
- **模型**：`Bot.DefaultModel` 只在输入未指定模型时生效；Run 快照记录实际模型。
- **TeamBot**：Mission + Roster；roster 的 `captain` 是有效 Bot，成员列表注入 captain
  prompt；TeamBot 不拥有 Session、不授予权限。
### 3.3 触发器交付语义（W10）

- **HMAC（webhook）**。`TriggerSpec.WebhookSecret` 是每个触发器的 HMAC-SHA256 密钥
  （至少 16 字节），`json:"-"`，绝不进入 `GET /v1/triggers`、插件快照或日志；生产用
  `WebhookSecretEnv` 指向环境变量（内置示例：`KEEL_EXAMPLE_WEBHOOK_SECRET`），注册时解析一次。
  签名覆盖**原始 body 字节**，canonical 为：

  ```
  v1\n<tenantId>\n<triggerId>\n<timestamp>\n<idempotencyKey>\n<rawBody>
  ```

  请求头：`X-Keel-Timestamp: <unix 秒>`、`X-Keel-Signature: sha256=<hex>`；
  另有 `X-Keel-Webhook: 1`（可省略，出现签名头即按 webhook 处理）。
  时间戳必须在 `webhookToleranceSec`（默认 300s，上限 86400s）内，签名用
  `hmac.Equal` 常量时间比较。
- **fail-closed**。未配置 secret 的 webhook 触发拒绝一切签名投递（403）；缺失/错误/过期签名
  返回 401。两者都写审计 `status:"rejected"`，错误只带类别（`missing_timestamp` /
  `stale_timestamp` / `bad_signature` 等），不带 secret 或收到的签名。手动 fire 是已认证的
  运维动作，不需要签名。
- **去重**。幂等键仍是唯一身份：同键并发按 key 串行化，进程内重复投递返回
  `deduped:true` 并审计 `status:"deduped"`；跨重启由 kernel 的 requestId 去重，返回
  `fired + deduped:true`。重复投递不会产生第二条 `user.message`。
- **漏跑补偿**。`trigger_state(tenant_id, trigger_id, last_scheduled_at, ...)` 持久化
  “已处理的最新计划窗口”。重启/注册时按该游标计算 `(last, now]` 内错过的窗口：
  只补 `catchUpLimit`（默认 3）个**最近的**窗口，且不早于 `catchUpWindow`（默认 24h）；
  更老的窗口合并跳过，审计 `status:"skipped"` 并记录 coalesced 数量。补跑使用与正常计划相同
  的稳定幂等键 `sched:<id>:<unix>`，所以即使补偿重复也不会二次提交。停机会话恢复不会
  产生风暴；上限内仍失败的窗口由审计可见。
- **并发上限**。`maxConcurrent`（默认 1）限制同一触发器同时在飞的 fire。超限返回
  `ErrTriggerBusy` → HTTP 429 + `Retry-After: 1`，审计 `status:"throttled"`；不排队，
  因为无界队列会把过载隐藏成延迟。同幂等键的重试先等待该键的首次 fire，再走去重分支，
  不会被当作超限。
- **初始任务卡**。一次成功 fire 在提交 `user.message` 后立即调用
  `tasklog.EnsureCard`，返回 `taskCardId` 并写审计；失败不回滚已提交的回合，而是返回
  `taskCardError`/审计字段（首次工具调用仍会懒创建）。满足 task-log「会话起步即有卡」。
- **观测**。`GET /v1/triggers` 的每个条目带 `fires/failures/dedupes/throttled/rejected/skipped`、
  `inFlight`、`webhookConfigured`、`lastScheduledAt` 与最近 20 条审计（fired/failed/deduped/
  throttled/rejected/skipped）。

### 3.3 触发器归属（A4.2b3a 补片）

- **归属**：每个 `TriggerSpec` MUST 带 `botId`；注册（插件装配）时校验 Bot 存在、未归档且
  属于 trigger 的 tenant。没有 `botId` 的 trigger 不注册；部署没有 Bot 归属 seam 时 fire
  fail closed（503），绝不创建无主 Session。
- **fire**：`TriggerEngine.Fire` 先解析 Bot → 用 `store.CreateOwnedSession` 在同一事务写
  Session、`bot_id/account_id/created_by_account_id` 与 owner 关系 → 再提交 `user.message`。
  输入的 `selection` 事实写 `{"botId": ...}`，与 Session 归属一致；attempt 因此按该 Bot 的
  Account ∩ Harness 解析 prompt/tools/model。`team_bot_id` 为空（Bot 触发器不是 TeamBot 启动）。
- **created_by**：手动 fire 记调用者 Account；`schedule` / `webhook` 记 Bot 自己的 Account
  （无人值守的 service actor）。审计条目同时记录 `botId` 与 `createdByAccountId`。
- **tenantless trigger**：manual/webhook 注册可以不带 tenant；它的 `botId` 在 fire 时按调用者
  tenant 解析，不存在跨租户或部署级全局 Bot。`interval`/`cron` 必须有 tenant，注册时即校验。
- **拒绝语义**：Bot 缺失/跨租户 → 403；Bot 已归档或 pinned Session 归属冲突 → 409；不落
  Session、不写 turn，审计记 `refused`。修复 Bot 后同一幂等键可重试。
- **示例插件**：`keel.example` 只在配置了 `--example-bot-id`（或 `KEEL_EXAMPLE_BOT_ID`）时声明
  trigger；先创建 Bot，再以该 Bot 启动 `keel serve`，`POST /v1/triggers/keel.example.ping/fire`
  才会以该 Bot 身份起 turn。未配置时示例插件只加载工具/hook，不注册 trigger。

## 4. 与内核的接缝

- `RunControl`（`agentext`）→ `agent.RunController` → worker：取消注册在**执行 attempt 的进程**；
  跨进程只能返回 501，不伪造成功。
- `TurnStarter`（`agentext`）→ kernel edge commit：触发器/subagent/手动触发共用一条写路径。
- `WrapStream`/`WrapTelemetry`（`servicehost`）：partial 生产者与 turn 遥测，不碰内核。
- 摘要/取消语义见 `docs/KERNEL.md` §2「运行摘要」。

## 5. 验收

`internal/agentapi`、`internal/agentext`、`cmd/keel` 均有 PG 集成测试；扩展面 E2E 覆盖
插件注册→触发→hook→subagent→取消在线 attempt。全量门禁见 `scripts/acceptance-all.sh`。
对外文档的术语纪律由 `scripts/naming-scan.sh` 扫描。
