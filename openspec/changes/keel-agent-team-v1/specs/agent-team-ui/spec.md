# agent-team-ui Specification

## ADDED Requirements

### Requirement: 设置是完整页面

设置 MUST 是客户端内的完整页面/路由，MUST NOT 继续以单一 modal 承载模型/Agent/团队配置。
页面 MUST 采用左侧分组导航 + 右侧内容区，并 MUST 支持深链到 section、Provider、Agent、
Team、插件能力。

分组 MUST 至少包含：

- **模型**：Model Providers / 凭据
- **Agents**：Agent 库 / 新建 Agent
- **AgentTeam**：团队预设 / 新建团队
- **工具**：工具注册源 / 能力
- **能力**：图像 / 视频 / 决策 / 语音（TTS、STT）
- **客户端**：外观 / 通知
- **插件**：已安装 / 市场 / 创造模式（能力树）
- **高级**：日志与诊断 / 更新

#### Scenario: 深链到团队
- **WHEN** 从会话团队选择器点击“管理团队”
- **THEN** 打开设置页 AgentTeam section 并定位当前团队，保留返回会话路径

### Requirement: Agent 编辑器

Agent 编辑器 MUST 提供：

- 名字、描述、头像（identicon 预览/seed 重生成/上传/URL 覆盖）
- 默认模型选择（model provider + model + 调参；只是默认）
- 工具选择器（从插件工具注册源搜索/分组/选择；显示权限与危险等级）
- 系统提示词编辑器（inline 或 AGENTS.md 文件引用）
- 团队 CLI 能力开关（enabled + 允许的子命令）
- 保存/重置/校验状态

默认文案 MUST 只是建议值；Agent 名称、描述、工具/提示词均可编辑，MUST NOT 写死。

#### Scenario: 新建 Agent 空态
- **WHEN** 用户打开新建 Agent
- **THEN** 看到名字输入、自动 identicon、默认模型选择与“工具/提示词可选”的渐进披露；
  不要求一次填完所有高级字段

#### Scenario: 工具注册源搜索
- **WHEN** 用户搜索工具
- **THEN** 结果来自注册源并按插件/权限分组；无匹配显示空态与清空搜索动作

### Requirement: 头像生成

头像编辑器 MUST 提供确定性 identicon 预览；相同 seed MUST 显示相同头像。MUST 支持
“换一个 seed”、上传图片、URL 覆盖；加载失败 MUST 回退 identicon，不得空白。

#### Scenario: identicon
- **WHEN** 用户不选择上传/URL
- **THEN** 头像由 Agent id/seed 本地生成，离线可用；复制 Agent 时默认沿用/可重生成 seed

### Requirement: AgentTeam 画布

团队编辑 MUST 以画布呈现：

- **captain 槽**：必填、恰好一个；只能替换，不能移除
- **成员列表**：0..N，每条显示 Agent 头像/名字/标签/描述/默认模型/状态；可添加已有 Agent
  或“新建 Agent”；可移除/替换/排序
- **团队默认**：工具覆盖/模型覆盖（可选）
- **Team CLI 能力面板**：enabled、允许的 member add/remove 等；关闭后说明影响
- 保存状态：valid/invalid；缺 captain 时阻止启动并给出修复入口
- **team 配置文件指示**：显示 `team.config` 的 revision/hash 与 DB-backed 语义；团队级
  覆盖 MUST 标记为 “team override”，并说明不会改写共享 model/agent provider 配置
- **Team CLI 能力面板**：显示 enabled 状态、允许的 member add/remove 子命令、关闭后的影响；
  关闭后 captain 的工具/CLI 面必须同步移除

成员标签/描述 MUST 可编辑；MUST NOT 把成员显示成固定“subagent 1/2/3”。

#### Scenario: 只配队长
- **WHEN** 团队只有 captain
- **THEN** 画布显示“成员可选，队长可通过 CLI 自行组装”；保存有效

#### Scenario: 缺队长
- **WHEN** 团队没有 captain
- **THEN** 画布突出 captain 槽，保存为 invalid，启动按钮禁用

### Requirement: 会话启动模式

新会话启动 MUST 先选择模式：

- **Standard**：选择模型（或默认 Agent）→ 开始；不显示团队选择。
- **AgentTeam**：选择有效团队 → 开始；显示 captain 与成员摘要；缺 captain 的团队禁用。

会话 header MUST 显示当前模式：Standard 显示模型；Team 显示团队名 + captain，并提供成员
抽屉/管理入口。运行中切换 MUST 说明只影响下一次 run。

#### Scenario: 标准模式
- **WHEN** 用户选择 Standard
- **THEN** 只看到模型选择/默认 Agent，不出现团队字段

#### Scenario: 团队模式
- **WHEN** 用户选择 AgentTeam
- **THEN** 只看到有效团队列表；选中后显示 captain/成员摘要并可启动

### Requirement: 插件能力树 UI

设置页/创造模式 MUST 以树形展示插件能力：

- CLI 命令路径与子命令
- summary/description
- 权限、danger（read/write/destructive）、idempotent
- callable_by（ui/agent/team_captain）
- 示例命令与复制

能力树 MUST 与 `--help`、manifest 同源；插件禁用/卸载后 MUST 同步移除，MUST NOT 留死命令。

#### Scenario: 查看团队 CLI
- **WHEN** 用户打开 `agent-team-plugin` 能力
- **THEN** 能看到 `keel team member add` 等子命令、危险等级与是否允许 captain 调用

### Requirement: 状态、可访问性与组件来源

所有组件 MUST 覆盖空/加载/错误/权限不足/成功五态；错误 MUST 给出可执行下一步。
MUST 支持键盘导航、focus-visible、可访问名称；390px MUST 单列可读，不得横向溢出。

UI MUST 使用 shadcn 官方组件与既有 solid-glass 材质；MUST NOT 手写仿制组件或第三方图标语言。
视觉验收 MUST 先过组件画廊（Agent 卡片/编辑器、头像、工具选择器、团队画布、成员卡、
Team CLI 面板、会话启动模式、能力树），再组合页面。

#### Scenario: 窄屏团队画布
- **WHEN** 390px 打开团队画布
- **THEN** captain 与成员单列堆叠，成员卡可展开，启动按钮可达

#### Scenario: 空团队库
- **WHEN** 租户还没有团队
- **THEN** 显示空态、说明“队长必选、成员可选”，并提供“创建团队”主操作
