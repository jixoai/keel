# plugin-commands

## ADDED Requirements

### Requirement: 命令目录是唯一发现真源

系统 SHALL 为每个会话发布命令目录（`GET /v1/sessions/{id}/commands`，需 `session.read`），
聚合内置命令、当前 tenant 已启用插件的 `provides.commands` 与已安装 skills；客户端 MUST
从该目录渲染命令，MUST NOT 本地维护第二份命令清单（`/learn` 作为已声明的客户端模板除外）。
目录 MUST 按“内置 → 插件（pluginId 字典序）→ skills”固定排序，并返回
`name/source/summary/argsSchema/danger/idempotent/targets/requiredPermissions/available`。
停用或卸载的插件命令 MUST NOT 出现；启用但 `PENDING`/`FAILED` 的命令 MUST 以
`available=false` 加稳定原因出现。目录 MUST 有上限（256 条）并在截断时给出 `truncated`。

#### Scenario: 停用插件命令消失

- **WHEN** 一个插件被停用或卸载
- **THEN** 下一次目录请求不再包含它的命令，执行入口返回 404

#### Scenario: 插件装配失败仍可见

- **WHEN** 插件已启用但运行时 `FAILED`
- **THEN** 目录列出其命令且 `available=false`、原因为 `plugin_failed`

#### Scenario: 跨租户目录

- **WHEN** 租户 B 请求租户 A 的会话命令目录
- **THEN** 返回 404，且不列出 A 的任何插件命令

### Requirement: 插件声明命令且不新增执行原语

插件 MUST 在 `plugin.yaml` 的 `provides.commands[]` 声明命令，字段 MUST 包含
`name/summary/tool/danger`，可选 `description/args_schema/idempotent/targets`。
`name` MUST 匹配 `^[a-z][a-z0-9-]{0,31}$`；发布名 MUST 为 `/<plugin-id>:<name>`，插件
MUST NOT 声明内置裸名。`tool` MUST 属于同一清单的 `provides.tools`，MUST NOT 指向其他
插件或宿主聚合工具；命令 MUST NOT 引入新的执行原语。`danger` MUST 是
`read|write|destructive`；`idempotent` 缺省 MUST 为 false；`args_schema` MUST 是
≤16 KiB 的 JSON object schema；`targets` 显式声明时 MUST 非空且属于 `web|native|tui`，
缺省 MUST 为三者全集。安装/更新
MUST 在字段级拒绝非法声明，并在审批中展示命令名、danger 与 targets；停用/卸载 MUST 在
同一 generation 撤销命令。

#### Scenario: 工具未声明

- **WHEN** `provides.commands[].tool` 不在同一插件的 `provides.tools`
- **THEN** 安装返回 `provides.commands[i].tool` 字段级错误，包不被采用

#### Scenario: 裸名冲突

- **WHEN** 插件声明 `name: compact`
- **THEN** 安装被拒绝并提示内置命令名保留

#### Scenario: 安装审批可见

- **WHEN** 审批一个带命令的插件安装
- **THEN** 审批内容包含每个命令的发布名、danger 与 targets

#### Scenario: 停用即撤销

- **WHEN** 停用一个提供 `/vendor.reindex:refresh` 的插件
- **THEN** 该命令从目录与执行入口同时撤销，不残留路由或处理器

### Requirement: 命令执行幂等且 durable

系统 SHALL 提供 `POST /v1/sessions/{id}/commands/{name}`（需 `session.write`），body
`{args, requestId}`；执行前 MUST 校验会话归属、命令存在、参数 schema 与 session idle。
同一 `requestId` MUST 幂等：重放 MUST 返回首次记录的结果，MUST NOT 二次执行工具或二次
打开 decision；并发同 requestId MUST 串行化。每次进入执行或 decision 的调用 MUST 产生一条
durable `command.invoked` 记录，含命令名、来源、actor、tenant/session、requestId、状态、
耗时、参数哈希与长度、可选的 `decisionId`；MUST NOT 记录参数正文、消息内容或密钥。
前置拒绝（未知命令/参数/权限/忙）按 §权限 的审计规则记录。命令结果 MUST NOT 自动进入
模型上下文。

#### Scenario: 重放不二次执行

- **WHEN** 客户端用同一 requestId 重发一个已成功的命令
- **THEN** 返回首次结果，插件工具/内置 seam 的调用次数不增加

#### Scenario: 忙时拒绝

- **WHEN** 会话有在途 turn 或压缩活动时执行命令
- **THEN** 返回 409 `session_busy`，不执行命令

#### Scenario: 审计不含参数

- **WHEN** 执行一个带敏感文本参数的命令
- **THEN** `command.invoked` 只含参数哈希与长度，不含正文

### Requirement: 命令不是 prompt 模板

命令 MUST 映射到一个宿主动作或一个已审批的插件工具；MUST NOT 通过拼接
`user.message`、system prompt 或 assistant 文本“实现”命令。只产生提示词文本的功能 MUST
保留为客户端模板或 skill/tool，MUST NOT 注册为命令。`/learn` MUST 保持客户端模板语义：
它不在命令目录中，仍通过普通输入发送 `/learn` 指令；`/skill:<name>` MAY 按引用加载技能
内容，但 MUST NOT 由客户端展开任意文本冒充命令。

#### Scenario: /learn 不在目录

- **WHEN** 打开命令目录或输入 `/`
- **THEN** 不出现 `learn` 命令；`/learn` 仍按既有模板路径发送

#### Scenario: 纯文本命令被拒

- **WHEN** 一个插件命令的实现只有“把固定文本塞进下一条 user.message”
- **THEN** 校验/评审拒绝该命令，要求改成模板、skill 或工具

### Requirement: 命令权限与审批 fail closed

命令目录与执行 MUST 按调用者有效权限判定：目录的 `requiredPermissions` MUST 包含执行
路由的 `session.write` 与命令自身 key；内置命令使用其声明的 key；插件命令 MUST
继承 `plugin.invoke`、目标工具在 registry 中声明的权限 key 与插件 `permissions.tools`；
插件命令的 `requiredPermissions` MUST 由工具 registry 派生，插件 MUST NOT 声明更宽的权限。
`danger=destructive` 的命令 MUST
先开 durable decision；拒绝 MUST 返回 403、超时 MUST 返回 408，且 MUST NOT 调用工具。
权限/审批失败 MUST 不产生部分副作用，并写入 `permission.denied` 或 `command.invoked` 的
拒绝记录；参数错误 MUST 返回字段级 400、忙 MUST 返回 409，均 MUST NOT 产生副作用。

#### Scenario: 缺权限

- **WHEN** 调用者没有命令所需的 `plugin.invoke` 或 `session.write`
- **THEN** 返回 403，目录中该命令 `available=false` 并给出缺失 key

#### Scenario: 破坏性命令被拒

- **WHEN** 审批人拒绝一个 `danger=destructive` 命令
- **THEN** 命令不执行、不产生领域事件，审计记录拒绝原因

#### Scenario: 审批超时

- **WHEN** destructive 命令的 decision 超时
- **THEN** 返回 408，工具调用次数为 0，审计记录 timeout

### Requirement: 内置命令复用既有实现

`/compact` MUST 与 `POST /v1/sessions/{id}/compact` 使用同一权限
（`session.write` + `model.invoke`）、同一手动压缩 seam（requestId 幂等、busy/策略关闭
语义不变），MUST NOT 写第二套压缩实现。`/skill:<name>` MUST 委托既有
skills loader，为下一轮强制加载该技能；技能不存在 MUST 返回 404；`skill.read` 缺失 MUST
返回 403；`disable-model-invocation` MUST NOT 阻止用户通过该命令加载。内置命令名 MUST 在
目录中唯一，插件 MUST NOT 覆盖。

#### Scenario: compact 委托

- **WHEN** 用户执行 `/compact`
- **THEN** 走与 `POST /v1/sessions/{id}/compact` 相同的 seam 与权限，返回相同幂等/busy 语义

#### Scenario: 缺失技能

- **WHEN** 用户执行 `/skill:not-installed`
- **THEN** 返回 404，不注入任何 prompt 内容

#### Scenario: 下一轮生效

- **WHEN** 用户执行 `/skill:release-checklist`
- **THEN** 下一轮请求按引用包含该技能内容，既有会话历史与在途状态不被改写

### Requirement: web 与 TUI 行为一致

web 与 TUI MUST 使用同一服务端目录、同一命令名解析与同一执行路由。Composer 在首个 token
输入 `/` 时 MUST 打开命令菜单；选择后，剩余文本 MUST 仅在该命令 schema 只有单个
`text` 字符串属性时作为 `args.text`，否则 MUST 展示由 `args_schema` 生成的参数表单，
MUST NOT 猜测参数。命令面板 MUST 增加“命令”分组，与 Composer 使用同一目录。界面 MUST
覆盖加载、空、错误、无权限、成功五态；`targets` 不含当前目标的命令 MUST 显示不可用原因，
MUST NOT 静默隐藏。执行结果 MUST 以 `command.invoked` 行呈现，MUST NOT 伪装成
user/assistant 消息。`targets` 只决定目录中的可用性，执行路由不接收 target，target
MUST NOT 被当作授权边界。客户端 MAY 为本标签页的未决 destructive 命令持久化
requestId 与 args 以支持切会话/刷新后的同 requestId 重放；MUST 在终态、登出或 401
时清除，MUST NOT 写入日志或审计；服务端审计仍只保存参数哈希与长度。

#### Scenario: 两端同序

- **WHEN** 同一会话分别在 web 与 TUI 打开命令菜单
- **THEN** 两端命令名、顺序与可用性一致

#### Scenario: 目标不支持

- **WHEN** 一个命令只声明 `targets: [web, native]` 并在 TUI 打开
- **THEN** 菜单显示该命令不可用及原因，不执行也不消失

#### Scenario: 参数表单

- **WHEN** 一个命令要求 `args.path` 与 `args.depth`
- **THEN** 客户端展示对应表单，不把整行剩余文本塞进 `text`

### Requirement: 失败隔离与可观测

命令失败 MUST 只影响本次调用：插件崩溃/工具错误 MUST 返回类型化 502，宿主与其他命令
不受影响；插件随后 `FAILED` 时目录 MUST 反映 `plugin_failed`。未知命令 404、参数错误
400、权限 403、忙 409、未装配 503；错误 MUST NOT 泄露其他租户或未授权资源。目录与执行
MUST 写结构化日志（命令名、来源、requestId、状态、耗时），MUST NOT 记录参数正文或密钥。

#### Scenario: 插件崩溃

- **WHEN** 一个插件命令执行时插件进程崩溃
- **THEN** 返回 502，宿主仍可用；目录下一次请求显示该插件 `plugin_failed`

#### Scenario: 未知命令

- **WHEN** 客户端执行一个不在目录中的名字
- **THEN** 返回 404，不执行任何回退或模糊匹配
