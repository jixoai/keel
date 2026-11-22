# 产品术语对表（Bot / TeamBot / Account / Role / Profile）

> 本文件是 **keel 仓** 的术语入口，只做对表与指针，不复制产品/商业词典正文。
> fde2e 主仓的 `PRODUCT-UNIVERSE.md`、`BUSINESS-MODEL.md` 是产品命名、层级与商业口径的
> 唯一真源；这两个文件不在 keel 仓，本 change 不跨仓改写。发现口径冲突时以主仓为准并另立
> change 修正 keel 文档。

## 1. 当前词汇（alpha 零兼容）

| 概念 | 定义 | 代码/接口落点 |
| --- | --- | --- |
| **Account** | 登录主体与权限主体；human / bot / service 共用一套模型与 permission-key | `accounts` 表；`/v1/accounts`；`internal/botteam.Account` |
| **Role** | 身份与灵魂：名字、Instruction、语气、默认行为；**不授予权限** | `roles` 表；`/v1/roles` |
| **Profile** | `Account + Role` 的绑定；Bot 通过 Profile 获得人格与权限上限 | `profiles` 表；`/v1/profiles` |
| **Bot** | 可运行的 AI 员工：`Profile + ModelKind + Harness(tools + Instruction + permissions)`；是权限主体 | `bots` 表；`/v1/bots`；`keel bot` |
| **TeamBot** | 团队目标/使命（Mission）+ Bot 引用名单（Roster）；**不是**权限主体，不拥有 Session/Account/Profile | `team_bots` 表；`/v1/team-bots`；`keel team-bot` |
| **Harness** | Bot 的运行环境：工具白名单 + 上下文（Instruction + permissions） | `internal/botteam.Harness`；`internal/harnessapi` |
| **Session / Run** | Session 属于一个 Bot → 一个 Account；Run 是内核执行周期，用户无感 | `sessions.bot_id/account_id/created_by_account_id` |

权限口径：有效权限 = `Account 权限 ∩ Harness permissions`；Role/Instruction 不授权；
TeamBot 不参与权限判定。权限 key、scope、ReBAC 关系元组的完整规则见
`openspec/changes/keel-bot-identity-access/design.md` §3。

## 2. 旧词对表（已删除，不保留别名）

| 旧词/旧入口 | 新词/新入口 | 处理 |
| --- | --- | --- |
| `Agent`（产品面） | `Bot` | 删除；API/CLI/UI/文档/表名全部改 Bot |
| `TeamAgent` / agent team | `TeamBot` | 删除；TeamBot = Mission + Roster |
| `/v1/agents` | `/v1/bots` | 旧路由 404，无重定向/别名 |
| `/v1/teams` | `/v1/team-bots` | 旧路由 404，无重定向/别名 |
| `keel agent` | `keel bot` | 旧子命令未知，明确失败 |
| `keel team` | `keel team-bot` | 旧子命令未知，明确失败 |
| `agents` / `agent_teams` 表 | `bots` / `team_bots` | 旧表删除；旧 schema 启动即拒绝 |
| `api_keys`（tenant/admin 角色） | `accounts` 密码 + `bot_api_keys` | 旧 key 401；Bot key 绑定 `(account_id, bot_id)` |
| `KEEL_SERVE_PASSWORD` / 单口令 cookie | 账号密码登录 + `KEEL_SECRET_KEY` 签名 cookie | 旧口令/旧 cookie 全部失效 |
| `Authorization: Bearer <passphrase>` | 人类 cookie + CSRF；机器 `Bearer keel_bot_...` | 旧口令 Bearer 401 |
| `X-Tenant-ID` 手工注入（`keel serve`） | 登录身份派生租户 | 中间件剥离客户端头；`cmd/runtime` 仍以 `X-Tenant-ID` 为运行时接口 |

## 3. PI 技术包例外（保留原名）

以下名字指 PI 的 loop / 工具执行 / runner，不是产品 Bot，**不得**随产品面改名：

- `internal/agentcore`
- `internal/agentloop`
- `internal/agenttool`
- `internal/agent`
- 线协议名 `AgentAPI` / `AgentBackend`（契约名，改名破坏客户端/适配器）

边界与理由见 `docs/ARCHITECTURE.md` §7 与 `docs/HARNESS.md` 开头的命名注记；
扫描规则由 `scripts/naming-scan.sh` 执行（PI 技术词与协议名在脚本里显式白名单）。

## 4. 登录、CLI 与迁移口径

- 人类：网页/CLI/TUI 用 `KEEL_ACCOUNT_USERNAME` + `KEEL_ACCOUNT_PASSWORD`（或
  `--password-file`）；密码不接受命令行 flag。
- 机器：`KEEL_AGENT_TOKEN=keel_bot_...`；key 只显示一次，可轮换/吊销，不能登录浏览器。
- CLI：`keel bot|team-bot|model|skills` 共用一套登录；写请求自动带 `X-Keel-CSRF`。
- 零兼容：旧 `agents`/`agent_teams`/`api_keys` 数据清空重建；旧 schema 启动即拒绝；
  不写迁移垫片。部署方需要留档时自行备份（`docs/RUNBOOK.md`「Harness 产品进程」）。
