# Proposal: keel-bot-identity-access

> 立项：Owner 2026-11-21。**破坏性变更**：账号密码登录、超级管理员、permission-key 权限系统、
> Account/Bot/TeamBot 领域模型。**Alpha 阶段零兼容**：不保留旧口令登录、旧 API key 语义、
> 旧 `agents`/`agent_teams` 模型、旧路由/CLI/UI 词汇，不写迁移垫片。

## Why

Keel 现在只有一个部署口令（`KEEL_SERVE_PASSWORD`）和一个租户（`KEEL_SERVE_TENANT`），
`api_keys` 只有 `tenant/admin` 两种角色；多账号、权限组、资源级授权都不存在。下一阶段要支持
多账号、权限分配、权限组预设，以及“AI 员工”作为独立主体运行，当前模型不够用：

1. **没有账号主体**：所有浏览器请求共享一个口令；机器客户端只有租户级 key，无法表达
   “谁在操作、谁能看哪条会话”。
2. **没有权限目录**：功能是否有权限、谁能授予、授予了哪些，都没有单一真源。
3. **没有资源级授权**：会话/Agent/团队只有 `tenant_id`，团队协作与临时分享只能靠手写判断。
4. **产品词汇错位**：`Agent` 在业内已经是“产品”概念；产品内部需要更准确的
   `Bot / TeamBot / Account / Role / Profile` 模型。
5. **AI 员工不是独立主体**：当前 Agent 只是配置文档，没有自己的账号、权限和归属，
   无法解释“它替谁干活、出了事谁负责”。

## What Changes

### 1. 词汇重置（破坏性）

- **删除产品词汇 `Agent`**。产品内部、API、CLI、UI、文档、表名一律使用 `Bot`；
  `TeamAgent` 改为 `TeamBot`。
- 保留内核技术词 `agentcore` / `agentloop` / `agenttool` 作为 PI loop 的技术名词；
  产品面/产品域包必须改为 Bot 语义，边界见 D4（已冻结）。
- `AgentAPI`、`/v1/agents`、`keel agent`、`agents` 表等产品面命名全部改为 Bot 语义。

### 2. 领域模型

```
Account 1─N Bot
Bot = Profile + ModelKind + Harness(tools + context(Instruction + permissions))
Profile = Account + Role
TeamBot = 多个 Bot 组装
Session → 属于一个 Bot → 属于一个 Account
```

- **Account**：登录主体与权限主体；人账号与 Bot 账号统一建模。
- **Role**：只提供身份与灵魂（名字、Instruction、语气、默认行为），**不授予权限**。
- **Profile**：Account + Role 的绑定；Bot 通过 Profile 获得权限与人格。
- **ModelKind**：模型类型（text / image / decision / realtime / …），决定底层工作方式。
  Bot 仍有一个**默认模型**（provider + model）；运行时可带参数覆盖默认模型；
  Session/Run 快照记录实际解析出的模型。
- **Harness**：给模型的环境，包含工具与上下文；上下文 = Instruction + permissions。
- **TeamBot**：一个团队目标/使命（Mission）+ Bot 引用名单（Roster）。TeamBot **不是权限主体**，
  没有自己的 Account/Profile，不拥有 Session；权限始终只在 Bot。TeamBot 是资源，由某个
  Account 拥有并受 `team_bot.*` 权限管理，但它不向成员授予任何权限。

### 3. 认证与账号（破坏性）

- 登录方式改为**账号 + 密码**；密码使用 Argon2id 哈希。
- `KEEL_SERVE_PASSWORD` / 单口令 cookie 登录**删除**，旧 cookie 全部失效。
- **Auth 可选**：关闭认证只允许 loopback，并映射为内置本机超管；公网绑定拒绝无认证启动。
- **超级管理员**：部署级账号，拥有全部 permission-key 与全部 scope；可跨租户；所有操作审计。
- **首个超管**：CLI bootstrap，一次性 setup token，首次登录强制改密；超管不可删除；
  忘记密码只能在本机用 CLI 恢复。形态见 L17。

### 4. 权限系统

- **permission-key**：粗粒度 `domain.action`（约 40 个）+ 少量高危细化
  （`secret.*` / `plugin.*` / `account.*` / `permission.*`）。
- **scope**：`own | team | tenant | all`，与 key 分开建模，不把 scope 烘进 key 名。
- **权限组**：一组 `(key, scope)` 的集合；Account 可关联多个，取并集；**只做加法**，
  不引入显式 deny，不做组嵌套。
- **资源级授权（ReBAC-lite）**：动作由 permission-key 判定；资源可见性由通用关系元组判定：
  `(resource_type, resource_id, relation, subject_type, subject_id)`，关系词表固定为
  `owner / team_member / viewer / editor`。handler 禁止再手写 owner/team 判断。
- **目录治理**：代码内唯一 permission registry；CI 枚举所有 HTTP 路由、工具、CLI 子命令、
  UI 动作，缺 key 或未知 key 直接失败；高危域可拆细。
- **Bot 有效权限 = Account 权限 ∩ Harness permissions**（L11）：Account 是上限，
  Harness permissions 是 Bot 自身的收窄；Role/Instruction 不授予权限。
- **TeamBot 不参与权限判定**（L12）：它只提供团队目标与 Bot 名单；Bot 仍按自己的
  Account/Harness 权限工作。Bot 调用 Bot 的权限链规则见 Open Decision D9。

### 5. 运行时与归属

- **Bot 级绑定 Profile**：一个 Bot 固定一个 Profile；该 Bot 的所有 Session 继承。
- **权限实时判定**：每次工具调用/写操作读当前 Account/权限组授权；Role/Instruction 变更
  通过 hook 向相关 Session 注入提示，不承担权限正确性。
- **人类调用 Bot 是能力授权**：调用者 MUST 持有 `bot.invoke`（按 Bot/关系授予）；
  通过授权后，Bot 以自己的 `Account 权限 ∩ Harness permissions` 执行，结果返回调用者；
  **不做调用者 ∩ Bot 的交集**。`bot.invoke` 是高危权限，必须精确授予、可撤销、全程审计。
- **Session 属于 Bot，Bot 属于 Account**：默认可见性由归属 + 关系元组决定；
  跨账号共享必须显式建立 `viewer/editor` 关系。
- **无人值守运行**（定时/Webhook/后台）：以 Bot 账号权限运行；绑定与触发器创建受
  `permission-key` 与“授予不得超过自己”不变量约束，不引入第二套委托机制。

### 6. 破坏性迁移

- 旧 `agents` / `agent_teams` 表、旧 `/v1/agents` `/v1/teams` 路由、旧 `keel agent` /
  `keel team` CLI、旧设置页“Agents/团队”文案全部替换。
- 旧口令登录、旧 cookie、旧 `api_keys.role` 语义全部删除；旧 key 不再可用。
- 旧 sessions/agents/teams 数据**清空重建**；旧 schema 启动即拒绝，不写迁移代码（L19）。
- 不写兼容层、不写双读、不写迁移垫片。

## Decisions Locked

| # | 决策 | 结论 |
|---|---|---|
| L1 | 账号租户边界 | Account 属于 tenant；超级管理员为部署级、可跨租户；单租户部署 = 一个 tenant + 一个 bootstrap 超管 |
| L2 | Bot 运行时权限主体 | Bot 是独立权限主体；`bot.invoke` 是调用授权边界；Bot 以自己的 Account ∩ Harness 权限执行，不做调用者交集 |
| L3 | 资源级授权 | ReBAC-lite 关系元组 + permission-key；关系词表 `owner/team_member/viewer/editor` |
| L4 | Profile 绑定粒度 | Bot 级绑定；权限每次工具调用实时判定；Role 变化用 hook 提示 |
| L5 | 权限组合 | 只做加法；无 deny；无组嵌套 |
| L6 | 超级管理员边界 | 全能：全部 key + 全部 scope，可看业务内容；所有操作审计 |
| L7 | permission-key 粒度 | 粗粒度 `domain.action` + 高危细化；registry + CI 覆盖率门 |
| L8 | 产品词汇 | 删除 `Agent`；`Bot` / `TeamBot`；`TeamAgent` → `TeamBot` |
| L9 | Bot 模型 | `Bot = Profile + ModelKind + Harness(tools + context(Instruction + permissions))` |
| L10 | 归属 | Account 拥有多个 Bot；Session 属于 Bot → Account；TeamBot 由多个 Bot 组装 |
| L11 | Bot 有效权限 | `Account 权限 ∩ Harness permissions`；Account 是上限，Harness 是收窄；Role 不授权 |
| L12 | TeamBot 不是权限主体 | TeamBot = Mission + Roster；不拥有 Account/Profile/Session，不向成员授予权限；权限只在 Bot |
| L13 | Bot 调用是能力授权 | `bot.invoke` 是授权边界；Bot 以自己的 Account ∩ Harness 权限执行，不做调用链交集；`bot.invoke` 高危、可撤销、审计 |
| L14 | 权限对称 | 人类账号能做的事，Bot 账号在拥有相同 permission-key 时也能做；没有 Bot 专属权限，也没有人类专属权限（登录方式除外） |
| L15 | ModelKind 与默认模型 | ModelKind 必填；Bot 有默认 provider/model；运行时可带参数覆盖；Session/Run 快照记录实际解析出的模型 |
| L16 | 重命名边界 | 产品面（表/API/CLI/UI/文档）与产品域包改为 Bot；PI 技术包 `agentcore`/`agentloop`/`agenttool`/`internal/agent` 保留，例外清单入 design |
| L17 | 首个超管与恢复 | CLI `keel admin bootstrap` 一次性 setup token；`local-only` 映射内置 local-admin 超管；忘记密码只能本机 CLI 重置；超管不可删除/禁用；全部审计 |
| L18 | Bot 凭据 | Bot 用绑定到具体 Bot 的 API key（`account_id + bot_id`）；不能登录浏览器；人类用密码；key 可轮换/吊销 |
| L19 | 旧数据 | 清空重建；旧 schema 拒绝启动；不写旧格式读取/迁移代码 |
| L20 | TeamBot 跨账号 | 不是专属规则：roster 只是引用；能否放入/调用由 `bot.invoke` 决定；无权限合并 |

## Decisions（D1–D9，全部冻结）

- **D1（已冻结）**：Bot 有效权限 = `Account 权限 ∩ Harness permissions`（L11）。
- **D2（已冻结）**：TeamBot 不是 Bot，也不是权限主体；它是 Mission + Roster（L12）。
- **D3（已冻结）**：ModelKind 必填 + Bot 默认模型；运行时可带参数覆盖默认模型；
  Session/Run 快照记录实际解析出的模型（L15）。
- **D4（已冻结）**：产品面 + 产品域包重命名；PI 技术包保留（L16）。
- **D5（已冻结）**：CLI bootstrap + local-only 隐式超管 + CLI 恢复（L17）。
- **D6（已冻结）**：旧数据清空重建（L19）。
- **D7（已冻结）**：Bot 用绑定到具体 Bot 的 API key，人类用密码（L18）。
- **D8（已冻结）**：不存在 TeamBot 专属的跨 Account 规则；roster 只是引用，能否放入/调用由
  `bot.invoke` 权限决定（L20）。
- **D9（已冻结）**：调用 Bot 是能力授权——`bot.invoke` 是边界；Bot 以自己的权限执行，
  不做调用链交集（L2/L13）。

## Impact

- **Schema**：`tenants` / `accounts` / `roles` / `profiles` / `bots` / `team_bots` /
  `permission_keys` / `permission_groups` / `permission_grants` / `resource_relations` /
  `auth_sessions` / `auth_events`；旧 `agents` / `agent_teams` / `api_keys` 退役。
- **Auth**：`cmd/keel` 登录、cookie、CLI 登录、`scripts/serve-local.sh`、部署文档。
- **API**：`/v1/auth/*`、`/v1/accounts`、`/v1/permission-*`、`/v1/bots`、`/v1/team-bots`；
  旧 `/v1/agents` `/v1/teams` 删除。
- **Access**：新增核心授权包（HTTP/工具/CLI/Agent spawn 共用）；kernel 只记录
  `actor_account_id`，不 import 权限包。
- **UI**：登录页改账号密码；设置页改 Account / Permission Group / Bot / TeamBot；
  `can()` 改为 permission-key 驱动；无权限动作隐藏或 403。
- **Docs/Specs**：`docs/HARNESS.md`、`docs/ARCHITECTURE.md`、`PRODUCT-UNIVERSE.md` 术语对表。
- **Tests**：权限矩阵、跨租户负控、未授权 invoke 负控、登录限速、CI 覆盖率门、旧入口 404 负控。

## Non-Goals

- 不做 OIDC / SAML / SCIM / MFA（后续 change）。
- 不做完整 Zanzibar / OpenFGA（只做固定关系词表的 ReBAC-lite）。
- 不做权限组嵌套与显式 deny。
- 不做旧数据兼容迁移（alpha 零兼容）。
