# skills

## ADDED Requirements

### Requirement: Agent Skills 规范

系统 SHALL 加载 Agent Skills 规范的 `SKILL.md`：frontmatter `name` / `description` /
`license` / `compatibility` / `metadata` / `allowed-tools` / `disable-model-invocation`，
加 Markdown 正文（可含 scripts/references/assets）。目录发现与 PI 一致：
`~/.agents/skills/`、`.agents/skills/`、项目/用户 skills 目录，递归查找 `SKILL.md`。

#### Scenario: 上游 skill 原样加载
- **WHEN** 放入一个未修改的上游 Agent Skills 目录
- **THEN** 被扫描、校验并出现在 skill 列表中

### Requirement: 渐进披露与 /skill:name

系统提示 MUST 只包含 skill 的 name、description 与 path；正文 MUST 在 `skill` 工具被调用时加载；
`/skill:name` MUST 强制加载（PI 语义）。

#### Scenario: 未调用不占上下文
- **WHEN** 会话未使用某个 skill
- **THEN** 该 skill 正文不出现在模型请求中

### Requirement: 租户隔离与版本

skill SHALL 按租户注册；内容以 hash 去重，版本不可变；租户之间 MUST NOT 互相读取。

#### Scenario: 同名不同内容
- **WHEN** 两个租户各有同名 skill
- **THEN** 各自只看到自己的版本

### Requirement: 工具权限

`allowed-tools` SHALL 与实际可用工具集取交集；越权调用 MUST 被拒绝并记录。

#### Scenario: 越权
- **WHEN** skill 试图调用未授权工具
- **THEN** 返回拒绝，工具不执行

### Requirement: 热更新

文件变更 SHALL 在下一轮生效；正在执行的 run MUST NOT 被中途替换。

#### Scenario: 编辑 skill
- **WHEN** 编辑 SKILL.md 后开始新一轮
- **THEN** 新内容生效，旧 run 使用旧快照
