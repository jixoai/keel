# Design: keel-bot-identity-access

> 本文件是 `proposal.md` 的实现级展开。**Alpha 阶段零兼容**：所有旧入口、旧词汇、旧凭证
> 一律删除，不写 shim、不写双读、不写迁移垫片。Open Decisions 未冻结前，不进入实现。

## 1. 领域模型

```
Account 1 ── N Bot
Bot = Profile + ModelKind + Harness(tools + context(Instruction + permissions))
Profile = Account + Role
TeamBot = Mission + Roster（团队目标/使命 + Bot 引用名单；不是权限主体）
Session → 属于一个 Bot → 属于一个 Account
```

| 实体 | 职责 | 不负责 |
|---|---|---|
| Account | 登录主体、权限主体、资源归属 | 人格、模型、工具 |
| Role | 身份与灵魂：名字、Instruction、语气、默认行为 | 任何权限 |
| Profile | Account + Role 的绑定 | 运行状态 |
| Bot | 一个可运行的 AI 员工：Profile + ModelKind + Harness | 登录会话 |
| TeamBot | 提供团队目标/使命与 Bot 名单（Roster） | 权限主体、Account/Profile、Session 归属、第二套执行拓扑 |
| Session | 某个 Bot 的一个上下文 | 模型/工具配置 |
| Run | 内核执行周期（实现细节，用户无感） | 产品概念 |

### 1.1 不变量

1. **每个 Session 必须属于且只属于一个 Bot；每个 Bot 必须属于且只属于一个 Account。**
   TeamBot 不拥有 Session；一次“团队运行”是多个 Bot Session 按 Mission/父子关系组成的
   运行视图，不是第三类 Session。
2. **Role 永不授予权限。** 权限只来自 Account 的 grant 与 Harness permissions。
3. **权限只做加法。** 有效 key 集合 = 直接 grant ∪ 所有权限组 grant；无 deny、无嵌套。
4. **资源访问 = 动作 key + 关系。** 有 key 只能说明“能做这个动作”；能不能对这条资源做，
   由关系元组决定。
5. **授予不得超过自己。** 任何 grant/bind 操作中，授予者必须持有被授予的 `(key, scope)`；
   超管除外（超管持有全部）。
6. **调用 Bot 是能力授权，不是权限合并。** 调用者必须持有 `bot.invoke`（按 Bot/关系授予）；
   通过授权后，Bot 以自己的 `Account 权限 ∩ Harness permissions` 执行，结果返回调用者；
   不做调用链交集。`bot.invoke` 是高危权限，必须精确授予、可撤销、审计。
7. **无人值守不得绕过授权者。** 定时/Webhook/后台任务以 Bot 账号权限运行；创建/绑定受
   permission-key 与不变量 5 约束，不引入第二套委托机制。
8. **权限对称。** 人类账号能做的事，Bot 账号在拥有相同 permission-key 时也能做；
   没有 Bot 专属权限，也没有人类专属权限（登录方式除外）。`grant ⊆ own` 对两者同样生效。

### 1.2 管家 Bot：权限对称的验收场景

Owner 场景（2026-11-21）：部署里一开始只有一个“管家 Bot”。用户对它说需求，它用自己的
Account 权限创建 Bot、创建 TeamBot（Mission + Roster）、配置权限组并启动团队。

这条场景 MUST 成立，且 MUST NOT 需要任何 Bot 专属代码路径：

1. 管家 Bot 有 `bot.create`、`team_bot.manage`、`permission.manage`、`account.read` 等 key；
2. 它创建成员 Bot（默认属于同一个 Account，或按 `account.create` 新建 Account）；
3. 它创建 TeamBot，写 mission 与 roster；
4. 它在 `grant ⊆ own` 约束下创建/分配权限组；
5. 它启动团队，成员 Bot 各自以自己的 Account/Harness 权限工作；
6. 全过程落审计，DAG 可视化。

如果某一步需要“只有人类能做”的特判，说明权限模型有漏洞；不允许加 Bot 专属旁路。

## 2. 账号与认证

### 2.1 Account

```sql
accounts (
  tenant_id uuid NOT NULL,
  account_id uuid NOT NULL,
  username text NOT NULL,           -- 登录名；租户内唯一
  display_name text NOT NULL,
  kind text NOT NULL,               -- human | bot | service
  password_hash text,               -- Argon2id；bot/service 可为空
  is_super_admin boolean NOT NULL DEFAULT false,
  disabled_at timestamptz,
  created_at, updated_at,
  PRIMARY KEY (tenant_id, account_id),
  UNIQUE (tenant_id, lower(username))
)
```

- `is_super_admin` 只允许部署 bootstrap 创建；普通 API 不能设置。
- Bot 账号 `kind=bot`，默认无密码；凭据是绑定到具体 Bot 的 API key（L18）。
- 删除账号为软删除/禁用；已有 Session/Bot 的归属保留，审计保留。

### 2.2 登录

- `POST /v1/auth/login {username, password}` → `Set-Cookie: keel_session=...`。
- 密码：Argon2id（memory 64MiB, iterations 3, parallelism 2），每账号随机 salt；
  参数写入代码常量，升级走新 change。
- Cookie：HttpOnly + Secure + SameSite=Lax；签名密钥来自 `KEEL_SECRET_KEY`（已有租户密钥库），
  不依赖口令。
- 会话表 `auth_sessions(session_id, tenant_id, account_id, issued_at, expires_at, revoked_at,
  user_agent_hash, ip_hash)`；登出/改密/禁用账号即时吊销。
- CSRF：双提交 cookie；所有非 GET `/v1` 写请求必须带 `X-Keel-CSRF`。
- 登录限速：按 username + IP 双维度滑动窗口；连续失败锁定（指数退避，上限 15 分钟）。
- 登录成功/失败、登出、改密、禁用、超管操作写 `auth_events`。

### 2.3 Auth 可选

- `--auth=required|local-only|off`（或配置文档）：
  - `required`：默认；所有 `/v1` 需要登录。
  - `local-only`：仅 loopback 可访问，未登录请求映射为内置 `local-admin` 超管；
    公网地址绑定直接拒绝启动。
  - `off`：仅测试/一次性容器；必须显式 `--insecure`，公网拒绝启动。
- `KEEL_SERVE_PASSWORD` 删除；`scripts/serve-local.sh` 改为读取 bootstrap 凭据或
  `KEEL_BOOTSTRAP_TOKEN`。
- 旧 cookie 因签名密钥/格式变化全部失效；旧 `Authorization: Bearer <passphrase>` 不再接受。

### 2.4 首个超管与恢复（L17）

- 首次启动且无 `is_super_admin` 时，`keel admin bootstrap` 生成一次性 setup token
  （打印到 stderr/写入受限文件；容器可由 `KEEL_BOOTSTRAP_TOKEN` 传入）。
- 浏览器用 setup token 设置用户名/密码；token 一次性、短 TTL。
- 超管不可删除、不可禁用；至少保留一个。
- 忘记密码：本机 `keel admin reset --username ...`（要求文件系统权限/本地 socket），
  不能走公网 API。
- 所有恢复操作写审计。

### 2.5 Bot 凭据（L18）

Bot 账号不登录浏览器，只使用 API key：

```sql
api_keys (
  tenant_id uuid NOT NULL,
  key_id uuid NOT NULL,
  key_hash bytea NOT NULL,        -- sha256(plaintext)
  key_prefix text NOT NULL,
  account_id uuid NOT NULL,       -- 权限主体
  bot_id uuid NOT NULL,           -- 该 key 只能以这个 Bot 身份行动
  label text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL,
  revoked_at timestamptz,
  last_used_at timestamptz,
  PRIMARY KEY (tenant_id, key_id),
  UNIQUE (key_hash)
)
```

- 明文 key 只在签发时返回一次；不落库、不落日志、不可回读。
- 认证后得到 `(account_id, bot_id)`；Bot 的有效权限 = 该 Account 的权限 ∩ Bot Harness。
- key 可轮换/吊销；吊销即时生效。
- 触发器、Webhook、后台任务使用 Bot key；审计记录 `bot_id + account_id`。
- 人类账号的密码登录与 Bot key 是两种登录方式，但权限模型完全相同（L14 的例外）。

## 3. 权限系统

### 3.1 permission-key

- 命名：`domain.action`。当前 registry（`internal/access/keys.go`，34 个）：
  - session：`session.read` / `.write` / `.delete` / `.share`
  - role/profile：`role.read` / `.manage`；`profile.read` / `.manage`
  - bot：`bot.read` / `.create` / `.manage` / `.bind` / `.invoke`
  - team_bot：`team_bot.read` / `.manage`
  - trigger：`trigger.read` / `.manage`
  - plugin：`plugin.read`；`plugin.lifecycle.install` / `.enable` / `.disable` / `.uninstall`
  - model：`model.read` / `.configure`
  - secret：`secret.read.metadata` / `secret.read.value`；`secret.write`
  - account：`account.read` / `.manage`；`account.credential.reset` / `account.permission.grant`
  - permission：`permission.read` / `.manage`
  - audit：`audit.read`
- 高危 key 已经是细化形态（`secret.read.metadata/value`、`plugin.lifecycle.*`、
  `account.credential.reset`、`account.permission.grant`、`bot.invoke`），不存在会误授强动作的粗粒度 key。
- 资源型 key（session/bot/team_bot/trigger）允许 `own|team|tenant|all`；租户型 key
  （role/profile/plugin/model/secret/account/permission/audit）只允许 `tenant|all`。
- 唯一 registry：`internal/access/keys.go` 导出 key 常量与元数据
  （domain、display name、risk、required relation、allowed scopes、default groups）。
- CI 覆盖率门：
  - 枚举 HTTP 路由表、工具 manifest、CLI 子命令、UI 动作；
  - 每个入口必须声明 required key；缺失/未知 key 失败；
  - registry 中无入口使用的 key 只警告。

### 3.2 scope 与 grant

```sql
permission_grants (
  tenant_id uuid NOT NULL,
  grant_id uuid NOT NULL,
  subject_type text NOT NULL,        -- account（直授）；组授权走 permission_group_grants
  subject_id uuid NOT NULL,
  permission_key text NOT NULL,
  scope text NOT NULL,               -- own | team | tenant | all
  granted_by uuid NOT NULL,
  created_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, grant_id)
)
```

- Account 可直接持有 grant；也可通过 `permission_group_members` 加入多个组。
- 有效 `(key, scope)` 集合 = 直接 grant ∪ 组 grant；同 key 取最大 scope（own < team < tenant < all）。
- **scope 不替代 relation**：资源级 key 的单资源判定始终要求 key + 关系元组；`tenant`/`all`
  不能让无关系的账号读到别人的会话。scope 的差异体现在委派/聚合/列表过滤，不体现在单资源放行。
- 列表/聚合端点 MUST 走引擎级 `VisibleResources` 过滤器（按 principal 的关系集计算可见集合），
  MUST NOT 在 handler 里用 `scope >= tenant` 跳过关系。
- 权限组：

```sql
permission_groups (tenant_id, group_id, name, builtin boolean, created_at, updated_at)
permission_group_members (tenant_id, group_id, account_id, added_by, created_at)
permission_group_grants (tenant_id, group_id, permission_key, scope)
```

- 内置组（建议）：`super-admin`（全部 key + all）、`tenant-admin`、`bot-operator`、
  `viewer`、`auditor`。具体清单在 spec 冻结时定。
- `grant ⊆ own`：除超管外，授予者必须持有 `(key, scope)`；组授予按组内最大 scope 检查。
- 超管：`is_super_admin` 直接短路为全部 key + all；每次超管操作写审计（含 reason）。

### 3.3 资源关系（ReBAC-lite）

```sql
resource_relations (
  tenant_id uuid NOT NULL,
  resource_type text NOT NULL,       -- session | bot | team_bot | task | ...
  resource_id text NOT NULL,         -- uuid 资源（session/task）或 text handle（bot/team_bot）
  relation text NOT NULL,            -- owner | team_member | viewer | editor
  subject_type text NOT NULL,        -- account | group | team_bot
  subject_id text NOT NULL,
  created_by uuid NOT NULL,
  created_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, resource_type, resource_id, relation, subject_type, subject_id)
)
```

- 关系词表固定四个；不递归、不重写。
- 判定：`Authorize(account, key, resource)` =
  1) 有 key（含 scope）；2) 存在所需关系（viewer 可读、editor 可写、owner 全权）。
- 资源创建时自动写 `owner` 关系（创建者 Account）；Session 的 owner = Bot 的 Account，
  但 `created_by` 单独记录发起人。
- 团队共享：给 `team_bot` 或 `group` 授 `viewer/editor`；成员资格由
  `permission_group_members` 或 TeamBot 成员关系提供，不另造表。
- 禁止在 handler 里写 `if owner == account`；全部走统一 evaluator。
- 列表端点禁止用 scope 短路关系；用 `VisibleResources` 返回可见 id 集合，再按正常分页查询。

### 3.4 判定点

- HTTP middleware：路由 → required key；资源 id 从 path 解析后查关系。
- 工具调用：工具 manifest 声明 required key；Bot 的 Harness permissions 与 Account 权限
  交集后再判。
- Bot 调用：`bot.invoke`（按 Bot/关系授予）+ 审计；被调用 Bot 以自己的
  `Account ∩ Harness` 权限执行，不做调用者/调用链交集。`bot.invoke` 是高危 key。
- CLI：与 HTTP 同一套 key；CLI 登录后按账号权限。
- UI：`can(key, resource)` 只做展示；后端仍是权威。无权限动作隐藏；直接访问返回 403。
- Kernel：只记录 `actor_account_id` 与 `bot_id`，不 import `internal/access`。

**文件契约（A4.2b2 裁决，2026-11-22）**：`/v1/files/*` 维持**租户级契约**，不写 per-file
owner relation。原因是上传路径在 httpapi 边缘只有 tenant 上下文、没有 access seam；blob 是
内容寻址且可被多个 session 重复引用，per-file 关系需要独立的文件账本与引用迁移，成本不属于本
切片。因此 `file.read`/`file.write` 的 scope 是租户边界，**不声称文件级隔离**；需要“只给某个
账号/会话看某个附件”的产品需求必须另立 change（建文件账本 + 关系元组），不得在 handler 里用
隐藏规则补洞。

**审计写入语义（A4.2a 实测，修正措辞）**：业务写先提交，grant/invoke/relation/super-admin
审计行随后追加；追加失败时接口返回 500，不静默 warn。二者**不是同一事务**：业务提交与审计
追加之间存在崩溃窗口（操作已发生但无审计行），登记为 P2（见 `tasks.md` A4.2b）。Bot/TeamBot
创建与 owner relation、Profile rebind 与 relation 迁移仍要求同事务，这条不受影响。

## 4. Bot 与 TeamBot

### 4.1 Bot

```json
{
  "botId": "support-1",
  "name": "客服一号",
  "profile": {"accountId": "...", "roleId": "..."},
  "modelKind": "text",
  "defaultModel": {"providerId": "magpie", "modelId": "bailian-token-plan/qwen3.8-flash"},
  "harness": {
    "tools": ["bash", "mcp", "tasks"],
    "context": {
      "instruction": "...",
      "permissions": ["session.read", "session.write"]
    }
  }
}
```

- `Profile` 是引用；Account 与 Role 都可复用。
- `ModelKind` 必填；Bot 有默认 `provider/model`；运行时参数可覆盖默认值；
  Session/Run 快照记录实际解析出的模型。
- `Harness.tools` 是白名单；实际工具面 = 部署 registry ∩ 白名单。
- `Harness.context.permissions` 是 Bot 自己的收窄；有效权限 = `Account 权限 ∩ Harness permissions`（L11）。

### 4.2 TeamBot

TeamBot 是**团队目标/使命 + Bot 名单**，不是权限主体：

```json
{
  "teamBotId": "support-team",
  "name": "客服团队",
  "mission": "在 24 小时内解决客户问题，无法解决时升级给人工。",
  "roster": [
    {"botId": "support-1", "role": "captain"},
    {"botId": "image-1", "role": "member"}
  ]
}
```

- TeamBot **没有** Account/Profile/ModelKind，**不拥有** Session，**不授予**任何权限。
- TeamBot 是资源：由某个 Account 拥有，受 `team_bot.read/manage` 权限管理。Roster 可以
  引用任何 Bot；**能否把某个 Bot 放进 roster / 调用它，由该 Account 是否持有该 Bot 的
  `bot.invoke` 决定**——不存在 TeamBot 专属的跨 Account 规则（L20）。
- 团队目标（mission）作为上下文注入 captain/成员的 Session；Bot 仍以自己的
  Account/Harness 权限工作。
- 成员 Bot 是 captain 的工具（`invoke-bot` / `create-bot` / `fork-bot`）；具体怎么用由
  AI 选择工具决定，产品不替它决定。
- 运行时视图（TeamRun/DAG）是账本投影：Mission 节点 + Bot/Session 节点 +
  create/fork/invoke 边；不是新的持久实体，也不是权限主体。
- 不引入“临时 Bot”概念：`create-bot` 创建的就是普通 Bot（属于某个 Account）；
  是否持久、是否入队由工具参数与权限决定，不做特殊生命周期。

### 4.3 Session 归属

- `sessions.bot_id` 必填；`sessions.account_id` 由 Bot 派生并冗余存储（便于查询/RLS）。
- `sessions.created_by_account_id` 记录发起人（人类或触发者）。
- `sessions.team_bot_id` / `session_mission` 记录这次会话所属的团队目标（可选，快照）。
- 删除 Bot 时：已有 Session 保留，Bot 归档；新 Session 拒绝。
- 跨 Account 共享：`resource_relations` 加 `viewer/editor`；默认只有 Bot 的 Account 可见。

## 5. API / CLI / UI 草案

### 5.1 API

- `POST /v1/auth/login`, `POST /v1/auth/logout`, `GET /v1/auth/session`,
  `POST /v1/auth/password`.
- `GET/POST/PATCH/DELETE /v1/accounts`, `POST /v1/accounts/{id}/disable`,
  `POST /v1/accounts/{id}/password-reset`.
- `GET/POST/PATCH/DELETE /v1/permission-groups`,
  `POST /v1/permission-groups/{id}/members`,
  `POST /v1/permission-groups/{id}/grants`.
- `GET/POST/PATCH/DELETE /v1/bots`; `POST /v1/bots/{id}/invoke`.
- `GET/POST/PATCH/DELETE /v1/team-bots`.
- `GET /v1/permissions`（registry + 当前账号有效权限）。
- 旧 `/v1/agents`, `/v1/teams` 返回 404（删除，不做重定向）。

### 5.2 CLI

- `keel auth login/logout/whoami`
- `keel account list/create/disable/reset-password`
- `keel permission group ...`
- `keel bot list/create/update/delete/start`
- `keel team-bot ...`
- 旧 `keel agent`, `keel team` 删除。

### 5.3 UI

- 登录页：用户名 + 密码；错误不区分“账号不存在/密码错误”。
- 设置 → 账号：列表、创建、禁用、重置密码、分配权限组。
- 设置 → 权限组：key 矩阵（按 domain 分组）、scope 选择、成员管理。
- 设置 → Bot：Profile、ModelKind、Harness 工具/Instruction/permissions。
- 设置 → TeamBot：成员 Bot、captain、共享关系。
- 无权限的导航/按钮隐藏；直接访问显示 403 页面（五态齐全）。

## 6. 迁移（Alpha 零兼容）

### 6.1 重命名边界（D4 A）

产品面与产品域包 MUST 改为 Bot 语义：

- 表：`agents` → `bots`，`agent_teams` → `team_bots`；相关列/索引同步。
- API：`/v1/agents` → `/v1/bots`，`/v1/teams` → `/v1/team-bots`。
- CLI：`keel agent` → `keel bot`，`keel team` → `keel team-bot`。
- UI/文档/错误文案：删除 `Agent`/`TeamAgent`。
- 产品域包：`internal/agentteam` → `internal/botteam`；`internal/agentapi` → `internal/harnessapi`；
  `internal/agentext` → `internal/harnessext`。

PI 技术包保留原名，并在 `docs/ARCHITECTURE.md` 写明例外：

- `internal/agentcore`、`internal/agentloop`、`internal/agenttool`、`internal/agent`
  指 PI 的 loop/工具执行/runner，不是产品 Bot。

1. 新 schema 启动时创建 `accounts` 等新表；旧 `agents`/`agent_teams`/`api_keys` 表删除或忽略。
2. 首次启动无超管 → 要求 bootstrap；不允许匿名进入。
3. 旧 Session/Agent/Team 数据：**清空重建**（L19）；旧 schema 拒绝启动，不写迁移代码。
   部署方需要留档时自行备份；产品不提供运行时迁移路径。
4. 旧 cookie/旧 API key：全部失效；文档明确写“升级即重新初始化账号”。
5. 旧路由/CLI/UI：删除，不保留 301/别名。
6. `docs/HARNESS.md`、`docs/ARCHITECTURE.md`、`README.md`、`site/docs` 同步词汇。

## 7. 测试与证据

- 单元：Argon2id、cookie 签名/过期/吊销、CSRF、限速、权限并集、scope 取最大、
  `grant ⊆ own`、关系判定、超管短路。
- 负控：跨租户读、低权限借高权限 Bot、无 key 路由、旧路由 404、旧 cookie 401、
  旧 API key 401、禁用账号登录失败。
- 集成：登录 → 建 Bot → 建 Session → 调工具 → 共享给另一账号 → 撤销 → 下一工具调用 403。
- CI：permission registry 覆盖率门、路由/工具/CLI/UI 清单快照。
- E2E：每步截图 + HTML 报告；至少两条真链路（人类调用、无人值守触发）。
- 证据目录：`openspec/changes/keel-bot-identity-access/evidence/`。

## 8. 风险

| 风险 | 缓解 |
|---|---|
| 重命名范围失控（包名/表/路由/文档） | D4 先冻结边界；按产品面 → 表/API → 包名分阶段 |
| 权限系统做成“装饰” | CI 覆盖率门 + 负控 + 统一 evaluator，禁止 handler 手写判断 |
| 超管单点/锁死 | 不可删除、CLI 恢复、bootstrap token 一次性、审计 |
| 调用者借高权限 Bot 取数据 | 接受为能力模型：`bot.invoke` 是高危权限，按 Bot/关系精确授予、可撤销、每次调用审计 |
| 无人值守提权 | `grant ⊆ own` + `bot.invoke` 高危授予 + 触发器创建审计 |
| 旧数据丢失 | alpha 明确零兼容：清空重建（L19）；旧库拒绝启动；备份由部署方负责 |
| Role/权限概念再次混淆 | Role 只读不授权；权限只在 Account/Harness；文档与 UI 文案钉死 |

## 9. A1 实现裁决（2026-11-21，已接受）

1. **ID 类型混合**：account/role/profile/group/grant 用 uuid；bot/team_bot 用 text handle
   （`support-1`，与旧 agent id 语法和产品展示一致）；`resource_relations.resource_id/subject_id`
   用 text，以同一张关系表同时命名 uuid 资源与 text 资源。
2. **TeamBot hash 不含 archived**：归档是生命周期而非内容；`ArchiveTeamBot` 不产生 stale hash。
   与旧 `agentteam` 把 archived 纳入 hash 不同，已在代码注释说明。
3. **`auth_sessions`/`auth_events` 仅建表 + RLS**：A2 增加持久化方法，A1 不越界。
4. **删除语义**：accounts 用 `disabled_at`；bots/team_bots 用 `archived`；roles/profiles/
   permission_groups 硬删除，被引用时 FK 返回 `ErrConflict`。
5. **权限授权单一化**：`permission_grants` 只用于**直接账号授权**（`subject_type=account`）；
   组授权只用 `permission_group_grants`。A3 store/API 必须拒绝 `subject_type=group` 的直接
   grant，避免两套组授权真值。
6. **`permission_grants` 多态 subject 无 FK**：A3 evaluator 对无法解析的 subject MUST fail closed；
   A3 增加引用校验/测试。
7. **`permission_groups` 租户内 `lower(name)` 唯一**：接受，避免同名组。
8. **`model_kind` 无 DB CHECK**：词汇由 `internal/botteam` 校验，保持可扩展；
   `accounts.kind` 有 DB CHECK。
9. **`permission_keys` 不建表**：A3 代码内 registry 是唯一真源。
