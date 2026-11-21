# Tasks: keel-bot-identity-access

> 顺序：先冻结 Open Decisions，再动 schema/认证；权限与 Bot 模型必须同批落地，不能先上一半。
> Alpha 零兼容：删除旧入口，不写 shim/双读/迁移垫片。每个阶段结束更新本文件并提交证据。

## D0 设计冻结（阻塞全部实现）

- [x] D1：Bot 有效权限 = `Account 权限 ∩ Harness permissions`（已冻结，L11）
- [x] D2：TeamBot 不是 Bot、不做权限主体；它是 Mission + Roster（已冻结，L12）
- [x] D3：ModelKind + Bot 默认模型；运行时可覆盖；快照记录实际模型（L15）
- [x] D4：产品面 + 产品域包重命名；PI 技术包保留（L16）
- [x] D5：CLI bootstrap + local-only 隐式超管 + CLI 恢复（L17）
- [x] D6：旧数据清空重建；旧 schema 拒绝启动（L19）
- [x] D7：Bot 用绑定到具体 Bot 的 API key；人类用密码（L18）
- [x] D8：无 TeamBot 专属跨 Account 规则；由 `bot.invoke` 决定（L20）
- [x] D9：调用 Bot 是能力授权——`bot.invoke` 边界；Bot 以自身权限执行，不做调用链交集（L2/L13）
- [ ] 冻结 `specs/identity-access/spec.md` 与权限 key 初始目录；偏离走新 change

## A1 Schema 与领域模型

- [x] 新表：`accounts` / `roles` / `profiles` / `bots` / `team_bots` /
      `permission_groups` / `permission_group_members` / `permission_group_grants` /
      `permission_grants` / `resource_relations` / `auth_sessions` / `auth_events` /
      `bot_api_keys`（`permission_keys` 不建表，A3 用代码内 registry）
- [ ] 旧表退役：`agents` / `agent_teams` / `api_keys`；不保留兼容视图（A4/A5）
- [ ] 重命名：`agents`→`bots`、`agent_teams`→`team_bots`、`/v1/agents`→`/v1/bots`、
      `keel agent`→`keel bot`；产品域包 `agentteam`→`botteam`、`agentapi`→`harnessapi`、
      `agentext`→`harnessext`；PI 技术包保留（L16）（A4/A5）
- [x] `sessions` 增可空 `bot_id` / `account_id` / `created_by_account_id` / `team_bot_id`；
      旧数据清空、非空收口在 A4（L19）
- [x] RLS：所有新表 ENABLE/FORCE ROW LEVEL SECURITY + tenant policy；目录级测试
- [x] 领域包：`Bot` / `TeamBot` / `Account` / `Role` / `Profile` 纯数据 + 校验 + 规范化
- [x] 架构测试：kernel 不 import access/accounts/bots

## A2 认证

- [x] Argon2id 密码哈希 + 参数常量 + 单元测试
- [x] `POST /v1/auth/login|logout|session|password`；签名 cookie（HttpOnly/Secure/SameSite）
- [x] CSRF 双提交；所有写请求校验
- [x] 登录限速/锁定/审计（username + IP）
- [x] `auth_sessions` 持久化；登出/改密/禁用即时吊销
- [x] 删除 `KEEL_SERVE_PASSWORD` 单口令路径、旧 cookie、旧 Bearer 口令
- [x] auth 模式 `required|local-only|off`；公网 + off 拒绝启动
- [x] bootstrap/token/首次改密/超管不可删除/CLI 恢复（L17）
- [x] Bot API key：`(account_id, bot_id)` 绑定、一次性明文、轮换/吊销、认证后以 Bot 身份行动（L18）
- [x] 旧入口负控：旧 cookie 401、旧 API key 401、旧口令 401
- [x] 改密端点失败节流（P2-3）：独立 limiter key，4 次失败 429
- [ ] 反代可信 IP / 限速（P2-4）：`KEEL_TRUST_PROXY` opt-in 或部署文档钉死反代侧限流
- [ ] 账号 HTTP `/v1/accounts`（A4 接权限后暴露；A2 只有 service/store）

## A3 权限引擎

- [x] `internal/access`：key registry、scope、grant 并集、`grant ⊆ own`、超管短路
- [x] ReBAC-lite：`resource_relations` 四关系 + 判定函数；无关系拒绝、无解析 subject fail closed
- [x] HTTP middleware：路由 → required key；资源关系解析（A4.2a；见 `evidence/A4.2a-route-matrix.md`）
- [ ] 工具调用：manifest required key + Bot Harness permissions 收窄（A4.2b/A6：运行时工具面尚未接，见 A4.2b 登记）
- [ ] Bot spawn / session start：`bot.invoke`（高危、审计）+ Bot 自身权限（HTTP 已接；运行时工具面留 A4.2b）
- [ ] CLI 与 HTTP 同一套 key；UI `can()` 接 registry（A4/L5）
- [x] 初始 permission key 目录（A4.2a 后 47 个，已落地）与内置权限组（已 seed）
- [x] CI 覆盖率门辅助：endpoint 描述 → 缺失/未知 key 报告
- [x] 审计：grant/revoke/deny/超管操作/关系变更；敏感值不落日志（A4.2a：追加失败返回 500，非同事务；崩溃窗口 P2 见 A4.2b）

## A4 Bot / TeamBot 产品面

### A4.1 后端接线（先做，阻塞 L5）

- [x] access 适配器：`GrantSource` 只取 account 直授；`GroupGrants` 走
      `permission_group_members` join `permission_group_grants`；`RelationSource` 解析 store；
      `ResolveSubject` 无法解析时 fail closed
- [x] 引擎级可见性过滤器 `VisibleResources`：列表/聚合端点只准用它，禁止 handler 用
      `scope >= tenant` 跳过 relation；补跨租户/无关系负控
- [x] 内置权限组种子：super-admin（全部 key + all）、tenant-admin、bot-operator、viewer、auditor
- [x] 超管写入白名单：只有 bootstrap 可创建 `is_super_admin`；普通 API 拒绝提升；始终保留至少一个
- [x] 覆盖率门非空断言：CI 要求 `Checked >= 已知路由总数下限`，空快照失败
- [x] store/access 的 scope/subject 词表交叉一致性测试（漂移 fail closed）
- [x] 审计落盘：grant/revoke/deny/超管操作/relation 变更；敏感值不落日志
- [x] HTTP `/v1/accounts`（列表/创建/详情/禁用/启用/重置密码/授权组）接 access
- [x] HTTP `/v1/bots`（CRUD/归档/Profile/Harness/试跑）接 access
- [x] HTTP `/v1/team-bots`（CRUD/归档/Mission/Roster/启动）接 access
- [x] Bot key 端点迁到 `/v1/bots/{id}/keys`；删 A2 临时 `/v1/auth/bot-keys`
- [x] `/v1/auth/whoami` 返回 Account/Bot 身份，不再返回旧 tenant role
- [x] Bot 级 Profile 绑定；Role 变更 hook 注入提示（HTTP Profile/Role CRUD 见 A4.2b2）
- [x] Session 属于 Bot → Account；创建时写 owner 关系与 `created_by_account_id`
- [x] 旧无主 session 原子认领；Bot invoke/TeamBot start/generic session 归属
- [x] **单资源 Session 路由接关系**：`/v1/sessions/{id}/events|input|run|decisions`、SSE、
      cancel/archive 统一走 `session.read/write|delete` + relation（A4.2a）
- [x] Profile/Role HTTP CRUD（A4.2b2）：`/v1/roles`、`/v1/profiles` 的列表/创建/详情/PATCH/删除；
      account/role 同租户存在性校验，Profile account rebind 与 Bot owner 关系迁移同事务，
      被引用删除 409，分页有界
- [x] 分页：accounts/groups/roles/profiles 有界 limit；bots/teams 候选窗口 500；keyset 留后续
- [x] 多租户内置组 seed（A4.2b2）：`POST /v1/tenants`、`/v1/admin/tenants`、`keel admin tenant create`
      均播种 5 个内置组（幂等）；旧 operator 面随 A4.2b 删除时，其 seed 调用一并移除
- [x] Bot/TeamBot 创建与 owner 关系同事务；Profile rebind 与旧 owner 关系迁移同事务（A4.2a）
- [x] 管理员重置密码与 CLI reset 对齐（A4.2b2）：`password.reset`、吊销全部会话、下次登录 must-change
- [ ] `bot.invoke` 关系阈值（viewer vs editor/owner）产品裁决

### A4.2a 全路由权限收口（已完成，复评 8.5/10 PASS WITH P2）

- [x] 代码内唯一路由矩阵：method+pattern → public/operator/key/held/tenant/resource；
      resource 型 key 强制声明 relation 来源（path/query）
- [x] 启动与测试枚举双 mux + A2 auth surface；缺项拒绝启动（172 declarations / 172 routes）
- [x] 默认拒绝：未声明 `/v1` 路径 404；`Access` 未接线时非 public 路由 503
- [x] 单资源 Session 全链（events/SSE/input/retry/run/cancel/archive/decisions/approvals/
      team-tasks/files/tasks/subagents/fork/resume）接 key + relation
- [x] `POST /v1/sessions` 无 botId 时创建即写当前账号 owner relation；带 botId 仍走 `bot.invoke`
- [x] 旧 `/v1/agents|teams` 加 bot/team_bot held-key 门（A4.2b 删除前不再零权限）
- [x] `GET /v1/triggers` 按租户过滤；fire/read 走 tenant scope key；bot-operator 预设同步改 tenant
- [x] providers/models/secrets/skills/plugins/MCP/media/files/notifications/app 均声明 key
- [x] Bot/TeamBot create + owner relation、Profile rebind + relation 迁移同事务
- [x] 审计 grant/invoke/relation/super-admin 写失败返回 500（业务先提交，非同事务；崩溃窗口登记 P2）
- [x] 直授 >500 报错不截断；`requireKey` 遇资源型/未知 key 直接 500
- [x] team start 重验整份 roster 的 `bot.invoke`；archived 成员下一次 start 409/403
- [x] sub-agent spawn/fork 子 Session 继承父 owner relation
- [x] `GET /v1/metrics` handler 增加 `requireAdmin`；`GET /metrics` 改为 public + 运维契约说明
- [x] 测试：矩阵/mux 对表、每类端点 200/403/404、跨租户、零 grant、旧路由、Session/trigger/
      secrets 负控、rebind 部分失败、>500 直授、roster archived/revoke
- [x] 证据：`evidence/A4.2a-route-matrix.md`
- [ ] captain→member 运行时工具调用 `bot.invoke`（A4.2b；当前无 Bot tool surface）

#### A4.2b 重命名与旧入口删除

- [ ] 删除产品词汇 `Agent`：API/CLI/UI/文档/表名统一为 Bot
- [ ] `TeamAgent` → `TeamBot`；TeamBot = Mission + Roster，不是权限主体（L12）
- [ ] 旧 `/v1/agents` `/v1/teams` 404；旧 `keel agent/team` 删除
- [ ] 旧 `api_keys`/`agents`/`agent_teams` 表退役；旧 `keel admin tenant/key` 删除
- [ ] 产品域包 `agentteam`→`botteam`、`agentapi`→`harnessapi`、`agentext`→`harnessext`
- [x] Profile/Role HTTP CRUD（A4.2b2 已完成）；管理端重置密码强制下次改密（A4.2b2 已完成）

### A4.2b 剩余安全开口（A4.2a 复评登记，勿在 A4.2a 做）

- [ ] **运行时工具面授权（最大开口）**：工具 manifest 声明 required key + Bot Harness 收窄；
      captain→member 的工具调用/子 Bot 启动检查 `bot.invoke`；撤销后下一次调用失效（A4.2b/A6 前必须收）
- [ ] 旧 `/v1/agents|teams` 租户全量披露：随重命名删除（当前仅 held-key 门，无资源关系）
- [x] notification token 路由（`/v1/notifications/{id}/open|pixel`，A4.2b2）：auth edge 在
      session/CSRF/must-change 之前放行这两条路径；token 校验留在 handler，无/错 token 401、
      不泄漏通知存在性；矩阵 public + “token 是权威”注释；E2E 覆盖无会话有效 token 与无效 token
- [x] `GET /v1/files/{id}`：**裁决为租户级文件契约**（2026-11-22）。上传在 httpapi 边缘只有
      tenant 上下文且没有 access seam；blob 可被多个 session 重复引用，per-file owner relation
      需要新的文件账本与引用迁移，成本不属于本切片。现契约 = 租户 `file.read`/`file.write`，
      不假装 per-file 隔离；共享/隔离需求另立 change。设计记录见 `design.md` §3.4
- [ ] P2 审计崩溃窗口：业务写已提交、审计行未追加时进程崩溃会留下无审计操作；用 outbox/
      同事务写入收敛（当前选择“写失败返回 500”；`bot.invoke` 关系阈值仍按 A4.1 待裁决项）
- [ ] **P2（A4.2b1 复核登记，勿在本片做）Bot / legacy Agent id-space 双轨**：运行时仍可
      `agentId` 选旧 agentteam 文档，新 Bot 是 `bots.bot_id`；两者语义/归属/工具面尚未合一。
      随 A4.2b 重命名收口，删除旧 id 空间后再统一 `bot.invoke` 目标解析。
- [ ] **P2（A4.2b1 复核登记）子 Session 归属 vs 成员自己的 Account**：当前 spawn/fork 子
      Session 继承父 Bot 归属（A4.2a），目标成员 Bot 不 rebind；只有子 Session 确实归属成员
      Bot 时才用其 Account∩Harness。A4.2b/A6 与 Bot invoke 路径统一时裁决。
- [ ] **P2（A4.2b1 复核登记）plugin UI bridge 第二执行面**：`keel.plugin.callTool` 直接
      `AgentTool.Execute`，不走 Runner 的 per-call toolauth；当前靠 bridge token + 插件
      自身 tool 声明收口。若插件工具可产生跨资源副作用，需在 bridge 执行前补同一授权。
- [ ] **P2（A4.2b1 复核登记）空 Harness = 全拒的产品语义**：`context.permissions` 为空时
      Bot 工具面全部拒绝（安全默认）。创建/保存 Bot 时需要明确告警或文档说明，避免运营
      误以为“空 = 继承全部”。

### A4.3 Web 登录与数据层（L5 前置）

- [ ] 登录页改 username+password + bootstrap/setup token + 强制改密
- [ ] CSRF token 注入所有写请求；旧 passphrase 表单删除
- [ ] CLI `agentteam.go`/`model.go`/`skills.go` 改账号登录或 Bot key，不再读 `KEEL_SERVE_PASSWORD`
- [ ] 账号/Bot/TeamBot/权限组数据层与 `can()` 接 registry

### A4.4 产品面（L5，等设计方向修订）

- [ ] 账号 / 权限组 / Bot / TeamBot / TeamRun DAG / 记录（审计）页面
- [ ] 五态齐全、无权限隐藏 + 403、四档 viewport、shadcn+solid-glass

## A5 迁移与词汇对表

- [ ] 启动时检测旧 schema：拒绝启动并给出重建指令（不自动迁移）
- [ ] `scripts/serve-local.sh` 改 bootstrap/账号登录；`.env` 模板更新
- [ ] `README.md` / `docs/HARNESS.md` / `docs/ARCHITECTURE.md` / `site/docs` 术语对表
- [ ] `PRODUCT-UNIVERSE.md` / `BUSINESS-MODEL.md` 同步 Bot/TeamBot/Account/Role
- [ ] 旧 cookie/key 失效说明；升级即重新初始化账号
- [ ] 删除旧测试/旧 fixture；新增权限矩阵 fixture

## A6 验收

- [ ] 单元/负控：密码、cookie、CSRF、限速、并集、scope、`grant ⊆ own`、跨租户、
      低权限借 Bot、旧入口
- [ ] 集成：登录 → 建 Bot → Session → 工具 → 共享 → 撤销 → 下一工具 403
- [ ] 管家 Bot E2E：一个有 `bot.create`/`team_bot.manage`/`permission.manage` 的 Bot
      创建成员 Bot + TeamBot（Mission/Roster）+ 权限组并启动团队；无 Bot 专属旁路
- [ ] 权限对称：同一 permission-key 下人类账号与 Bot 账号行为一致；只允许登录方式不同
- [ ] 无人值守：触发器/Webhook 以 Bot 账号运行；`grant ⊆ own` 负控
- [ ] CI 覆盖率门全绿；路由/工具/CLI/UI 清单快照
- [ ] E2E 每步截图 + HTML 报告；人类调用 + 无人值守两条真链路
- [ ] 独立 A/B 复核，P0/P1 清零
- [ ] `openspec validate keel-bot-identity-access --strict`
- [ ] 证据目录 `evidence/`；任务板勾选；归档
