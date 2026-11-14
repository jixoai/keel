# agent-registry Specification

## ADDED Requirements

### Requirement: Agent 是完整智能体配置

Agent MUST 表示一个完整智能体，而不是 subagent 参数或一次工具调用。Agent MUST 至少包含：

- 稳定 id、可编辑 name、可编辑 description
- 头像（默认确定性 identicon，可上传/URL 覆盖）
- 默认模型绑定（model provider + model + tuning；只是默认，可被会话/任务覆盖）
- 工具能力（从插件工具注册源选择，保存 toolId + 开关/配置）
- 系统提示词（inline 或文件引用，例如 AGENTS.md）
- 团队 CLI 能力开关与允许的子命令

Agent MAY 额外包含技能、标签、权限、运行时偏好等扩展字段。

#### Scenario: 新建 Agent
- **WHEN** 用户创建一个 Agent，只填名字并选择默认模型
- **THEN** 系统生成稳定 id 和确定性 identicon；Agent 可用于团队/会话；
  工具与系统提示词为空时使用默认值，不强制用户先填满

### Requirement: 默认模型不是固定模型

Agent 的 `defaultModel` MUST 只是默认解析结果；会话、任务、团队 MUST 能显式覆盖。
实现 MUST NOT 把 Agent 写死到某个模型，也 MUST NOT 在 Agent 配置里内联明文凭据。

#### Scenario: 会话覆盖默认模型
- **WHEN** Agent 默认模型是 `deepseek-v4.1`，会话显式选择另一个模型
- **THEN** 本次 run 使用会话选择；Agent 配置不变

### Requirement: 工具能力来自注册源

Agent 的工具列表 MUST 来自插件提供的工具注册源（toolId、来源插件、schema、权限、危险等级、
可调用性）。UI MUST NOT 维护硬编码工具清单，Agent 配置 MUST NOT 内联工具实现。

保存 Agent 时 MUST 校验 toolId 存在且当前租户可用；插件卸载后引用 MUST 标记为不可用并给出
修复动作，MUST NOT 静默删除用户配置。

#### Scenario: 插件卸载
- **WHEN** 一个被 Agent 引用的工具插件被卸载
- **THEN** Agent 仍保留 toolId 引用，状态显示“工具不可用”，并提示重新安装或替换；
  运行时该工具不进入工具面

### Requirement: 头像默认确定性 identicon

Agent 头像 MUST 支持三种来源：`identicon`（默认）、`upload`、`url`。identicon MUST 由
`seed`（默认 agent id）在本地确定性生成（GitHub 风格网格），相同 seed MUST 得到相同头像，
不得依赖外部头像服务。

UI MUST 提供 seed 重生成、上传与 URL 覆盖；头像加载失败 MUST 回退 identicon，不得空白。

#### Scenario: 相同 seed 相同头像
- **WHEN** 两个 Agent 使用相同 avatar seed
- **THEN** 生成的头像像素一致；修改名字不会隐式改变 seed，除非用户显式重生成

### Requirement: Agent CLI 管理能力

agent-plugin MUST 提供可发现的 CLI 能力树，至少包含：

- `keel agent list` / `show`
- `keel agent create` / `update` / `delete`
- `keel agent start` / `stop`

每个子命令 MUST 有 summary、description、参数/输出 schema、权限、danger、idempotent、
callable_by。CLI 的写操作 MUST 与 UI 使用同一校验与租户边界；MUST NOT 出现只能启动、
不能管理 Agent 的残缺能力。

- 创建请求 MAY 省略 id（只填名字）：系统 MUST 生成稳定 id（`agent_` + 无横线 UUID）与
  以该 id 为 seed 的确定性 identicon。更新请求 MUST 携带 id，MUST NOT 静默生成新身份。
- 保存 Agent 时 MUST 校验每个 toolId 在当前租户的工具注册源中存在；不存在的引用 MUST
  保留并以 `tool_not_found` 报告，MUST NOT 静默删除。

#### Scenario: CLI 管理 Agent
- **WHEN** 用户运行 `keel agent update <id> --name ...`
- **THEN** 更新通过同一 schema 校验，UI 在下一次读取时看到新值；凭据仍只显示引用

### Requirement: Agent 引用与删除保护

Agent 被团队或会话引用时 MUST NOT 被硬删除；只能归档/禁用，并 MUST 返回引用方。
归档 Agent MUST 保留历史快照可读，MUST NOT 影响已启动 run。

#### Scenario: 删除被团队引用的 Agent
- **WHEN** 用户删除一个团队成员 Agent
- **THEN** 删除被拒绝并列出团队；UI 提供“从团队移除后删除”或“归档”动作
