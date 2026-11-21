# Identity Access（账号、Bot 与权限）

本 spec 定义 Keel 的账号主体、Bot/TeamBot 领域词汇、permission-key 权限系统与资源级授权。
**Alpha 阶段零兼容**：旧口令登录、旧 API key 角色、旧 Agent/TeamAgent 词汇与路由全部删除。
Open Decisions（见 `design.md`）未冻结前，本 spec 不覆盖对应实现细节。

## ADDED Requirements

### Requirement: 账号是唯一的登录与权限主体

系统 MUST 以 Account 作为登录主体与权限主体。每个 Account MUST 属于一个 tenant；
用户名在租户内 MUST 唯一。Account MUST 支持 `human`、`bot`、`service` 三种 kind。
密码登录 MUST 使用 Argon2id 哈希与每账号随机 salt；明文密码 MUST NOT 落库、落日志或回显。
登录成功 MUST 建立服务端可吊销的会话；登出、改密、禁用账号 MUST 立即使会话失效。

#### Scenario: 账号密码登录
- **WHEN** 一个启用的账号用正确用户名与密码调用 `POST /v1/auth/login`
- **THEN** 返回 200 并设置 HttpOnly/Secure/SameSite 会话 cookie
- **AND** 响应体不包含密码哈希、盐或会话签名密钥

#### Scenario: 错误凭据不泄露账号存在性
- **WHEN** 用户名不存在或密码错误
- **THEN** 两种情况返回相同的 401 响应形状与耗时量级
- **AND** 失败写入 `auth_events`

#### Scenario: 禁用账号即时失效
- **GIVEN** 账号 A 已登录且持有有效会话
- **WHEN** 管理员禁用 A
- **THEN** A 的下一个请求返回 401
- **AND** 该账号的所有 `auth_sessions` 被标记吊销

### Requirement: 认证可选但不得成为公网后门

系统 MUST 支持 `required`、`local-only`、`off` 三种认证模式。`local-only` 与 `off` MUST 只
接受 loopback 访问；绑定非 loopback 地址且认证关闭时，进程 MUST 拒绝启动。`local-only`
下未登录请求 MUST 映射为内置 `local-admin` 超管；`off` MUST 需要显式不安全开关。
旧的 `KEEL_SERVE_PASSWORD` 单口令登录 MUST 删除，旧 cookie MUST 失效。

#### Scenario: 公网绑定拒绝无认证启动
- **WHEN** `keel serve` 绑定 `0.0.0.0` 且认证模式为 `local-only` 或 `off`
- **THEN** 进程启动失败并输出明确错误

#### Scenario: 旧口令不再可用
- **WHEN** 客户端用 `Authorization: Bearer <旧 KEEL_SERVE_PASSWORD>` 请求 `/v1`
- **THEN** 返回 401，不建立会话

### Requirement: 超级管理员是部署级全权主体

系统 MUST 支持部署级超级管理员。超级管理员 MUST 拥有全部 permission-key 与全部 scope，
MAY 跨租户操作，MUST 能管理账号、权限组、Bot 与 TeamBot。超级管理员 MUST 不可删除、
不可禁用；系统 MUST 始终保留至少一个超级管理员。超级管理员的每次跨租户操作、授权、
改密与 impersonation MUST 写入审计。普通 API MUST NOT 创建或提升超级管理员。

#### Scenario: 超管不可删除
- **WHEN** 任何请求尝试删除或禁用最后一个超级管理员
- **THEN** 返回 409/403，账号保持启用

#### Scenario: 超管操作可追溯
- **WHEN** 超级管理员读取另一个租户的会话或修改其权限
- **THEN** `auth_events` 记录操作者、目标租户、动作、时间与结果

### Requirement: 产品领域只使用 Bot 与 TeamBot

产品内部、API、CLI、UI、文档与数据库表 MUST 使用 `Bot` 与 `TeamBot`，MUST NOT 使用
`Agent` 或 `TeamAgent` 指代产品实体。Bot MUST 由 Profile、ModelKind 与 Harness 组成：
Profile 提供 Account 与 Role；ModelKind 表示模型类型；Harness 提供工具与上下文
（Instruction + permissions）。Role MUST 只提供身份与灵魂，MUST NOT 授予权限。
Account MUST 拥有多个 Bot；TeamBot MUST 由多个 Bot 组装。

#### Scenario: 旧词汇入口被删除
- **WHEN** 客户端请求 `/v1/agents` 或执行 `keel agent`
- **THEN** 返回 404/未知命令，不存在重定向或别名

#### Scenario: Role 不授予权限
- **GIVEN** 一个 Bot 的 Role 文本声明“可以管理密钥”
- **WHEN** 该 Bot 尝试调用密钥管理工具
- **THEN** 权限判定只看 Account 与 Harness permissions，Role 文本不产生授权

### Requirement: permission-key 是功能的唯一权限词汇

系统 MUST 维护一份代码内唯一的 permission-key registry。每个 HTTP 路由、工具、CLI 子命令
与 UI 动作 MUST 声明其所需 permission-key。CI MUST 枚举这些入口并拒绝缺失或未知 key。
permission-key MUST 采用 `domain.action` 命名；scope MUST 独立建模为
`own | team | tenant | all`，MUST NOT 烘进 key 名。高危域（secret/plugin/account/permission）
MAY 使用更细的 key。

#### Scenario: 未登记 key 的路由使 CI 失败
- **WHEN** 新增一条 `/v1` 路由但没有声明 required permission-key
- **THEN** 权限覆盖率测试失败并指出该路由

#### Scenario: 未知 key 使启动失败
- **WHEN** 账号或权限组引用 registry 中不存在的 key
- **THEN** 写入被拒绝，进程启动时配置校验失败

### Requirement: 权限只做加法

有效权限 MUST 是账号直接 grant 与所有权限组 grant 的并集；同一 key 的多个 scope MUST 取
最大 scope（`own < team < tenant < all`）。系统 MUST NOT 支持显式 deny，MUST NOT 支持权限组
嵌套。授予者 MUST 只能授出自己持有的 `(key, scope)`；超级管理员除外。

#### Scenario: 多组取并集
- **GIVEN** 账号 A 属于组 G1（`session.read` + own）与 G2（`session.read` + tenant）
- **WHEN** A 读取同租户另一个账号的会话
- **THEN** 允许，因为并集取 `tenant` scope

#### Scenario: 授予不得超过自己
- **GIVEN** 账号 A 只有 `session.read` + own
- **WHEN** A 尝试给账号 B 授予 `secret.read` 或 `session.read` + tenant
- **THEN** 返回 403，grant 不写入

### Requirement: 资源访问由关系元组判定

系统 MUST 用通用关系元组判定资源级访问：
`(resource_type, resource_id, relation, subject_type, subject_id)`。关系词表 MUST 固定为
`owner / team_member / viewer / editor`，MUST NOT 递归重写。动作权限与资源关系 MUST 同时
满足；handler MUST NOT 手写 owner/team 判断。资源创建时 MUST 自动写入创建者的 `owner` 关系。

#### Scenario: 有 key 但无关系仍拒绝
- **GIVEN** 账号 A 持有 `session.read` + tenant
- **AND** 会话 S 属于账号 B，且 A 与 S 没有 viewer/editor/owner 关系
- **WHEN** A 读取 S
- **THEN** 返回 403

#### Scenario: 显式分享后可读
- **WHEN** S 的 owner 给 A 写入 `viewer` 关系
- **THEN** A 读取 S 成功；A 仍不能写入，除非另有 `editor`

### Requirement: Session 属于 Bot，Bot 属于 Account

每个 Session MUST 属于且只属于一个 Bot；每个 Bot MUST 属于且只属于一个 Account。
Session MUST 记录 `bot_id`、所属 `account_id` 与 `created_by_account_id`。默认可见性 MUST
只授予 Bot 的 Account；跨账号共享 MUST 通过显式关系元组。删除 Bot MUST NOT 删除已有
Session；Bot 归档后 MUST 拒绝新 Session。

#### Scenario: 默认私有
- **GIVEN** 账号 A 的 Bot 创建 Session S
- **WHEN** 同租户账号 B（无关系）读取 S
- **THEN** 返回 403

#### Scenario: 共享后撤销
- **GIVEN** A 给 B 写入 S 的 `viewer` 关系
- **WHEN** A 撤销该关系后 B 再次读取 S
- **THEN** 返回 403

### Requirement: 调用 Bot 是能力授权

调用 Bot MUST 需要 `bot.invoke`，按 Bot 或关系授予。通过授权后，被调用 Bot MUST 以自己的
`Account 权限 ∩ Harness permissions` 执行，MUST NOT 与调用者权限求交集，也 MUST NOT 沿
调用链收窄。`bot.invoke` MUST 被视为高危权限：授予、撤销与每次调用（调用者、被调用 Bot、
结果状态）MUST 审计。超级管理员权限 MUST NOT 委托给 Bot。无人值守运行（定时、Webhook、
后台任务）MUST 以 Bot 账号权限运行；创建绑定与触发器 MUST 受 `grant ⊆ own` 约束。

#### Scenario: 有 invoke 权限即可使用 Bot 自己的能力
- **GIVEN** 用户 A 持有 Bot X 的 `bot.invoke`
- **AND** Bot X 的账号拥有 `secret.read`
- **WHEN** A 调用 X 并要求读取密钥
- **THEN** X 以自己的权限执行并返回结果
- **AND** 审计记录调用者 A、被调用 Bot X 与结果状态

#### Scenario: 没有 invoke 权限被拒绝
- **GIVEN** 用户 A 没有 Bot X 的 `bot.invoke`
- **WHEN** A 调用 X
- **THEN** 返回 403，X 不启动

### Requirement: Alpha 零兼容

本 change MUST NOT 为旧口令登录、旧 cookie、旧 API key 角色、旧 `agents`/`agent_teams`
模型、旧 `/v1/agents` `/v1/teams` 路由或旧 CLI 词汇保留兼容层、双读、重定向或迁移垫片。
启动时发现旧 schema MUST 拒绝启动并输出重建指令。旧数据是否清空或接管 MUST 由
`design.md` 的 D6 决定；无论哪种，MUST NOT 运行旧代码路径。

#### Scenario: 旧 schema 拒绝启动
- **WHEN** 数据库存在旧 `agents` 表且没有新 `bots` 表
- **THEN** `keel serve` 启动失败并提示重建数据库

#### Scenario: 旧路由不存在
- **WHEN** 客户端请求 `/v1/agents` 或 `/v1/teams`
- **THEN** 返回 404，不返回旧数据
