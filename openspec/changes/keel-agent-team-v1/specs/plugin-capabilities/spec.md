# plugin-capabilities Specification

## ADDED Requirements

### Requirement: 每个插件声明能力树

每个插件 MUST 声明自己的能力与子能力（CLI/sub-command），包括模型、Agent、团队、工具类插件。
能力树 MUST 是机器可读的（manifest / `extensibility.json`），并与 `--help`、设置页、
创造模式目录同源。

每个能力节点 MUST 包含：

- `path`：稳定命令路径（如 `["model","provider","list"]`）
- `summary` 与 `description`：人能读懂的用途与边界
- `args_schema` / `output_schema`：参数与输出形状
- `permissions`：调用所需权限
- `danger`：`read` / `write` / `destructive`
- `idempotent`：是否幂等
- `callable_by`：`ui` / `agent` / `team_captain` 等可调用者
- `examples`（SHOULD）：至少一个人读示例

#### Scenario: 展示能力树
- **WHEN** 设置页/创造模式读取 `keel plugin capabilities --json`
- **THEN** 能看到 `keel model provider add`、`keel agent update`、`keel team member add`
  等完整命令树、描述、权限与危险等级，不需要猜命令

### Requirement: 模型插件 CLI

model-plugin MUST 提供 CLI 管理模型，而不仅是模型调用：

- `keel model provider list|show|add|update|remove|test`
- `keel model catalog <provider-id> [--refresh]`
- `keel model capability`（可选：类型/参数 schema）

写操作 MUST 与 UI 使用同一 schema 与租户校验；凭据 MUST 只接受引用，不接受明文回显。

#### Scenario: CLI 新增 Provider
- **WHEN** 用户运行 `keel model provider add --type image --credential-ref ...`
- **THEN** Provider 出现在 UI 列表；`keel model provider test` 可做健康检查

### Requirement: Agent 插件 CLI

agent-plugin MUST 提供 `keel agent list|show|create|update|delete|start|stop`。
CLI MUST 能管理 Agent 配置（名字/头像/默认模型/工具/AGENTS.md），不得只支持启动。

#### Scenario: CLI 查看 Agent
- **WHEN** 用户运行 `keel agent show <id>`
- **THEN** 输出 Agent 配置摘要、默认模型引用、工具清单与来源、团队 CLI 能力，
  不输出明文凭据

### Requirement: AgentTeam 插件 CLI

agent-team-plugin MUST 提供 `keel team list|show|create|update|delete|member add|remove|list|save|start`。
每个子命令 MUST 在能力树中声明；队长可调用的子命令由团队能力开关控制，且能力树与
`teamCli.commands` 白名单 MUST 双向覆盖（不得出现可授权但不存在的死命令）。

- team 配置是 DB-backed 文件；CLI 变更 MUST 修改 team 配置，MUST NOT 改写 model/agent provider 的共享配置；session 使用启动时快照。
- 关闭队长的 `agent-team` CLI 能力 MUST 从工具/CLI 面移除这些命令，而不是仅提示词劝阻；
  直接调用 MUST 返回 typed permission error。

#### Scenario: 队长自建成员
- **WHEN** 队长的团队 CLI 能力开启且允许 member.add
- **THEN** 队长可运行 `keel team member add ...` 创建成员，并把成员纳入当前团队运行实例

#### Scenario: 关闭队长 CLI
- **WHEN** 团队配置关闭 `teamCli.enabled`
- **THEN** 队长的工具/CLI 面不再出现 `keel team member ...`；调用返回 typed permission error

### Requirement: 能力生命周期与一致性

插件安装/启用/禁用/卸载时，能力树 MUST 同步增删；UI、CLI、Agent 工具面 MUST NOT 留下
死命令或幽灵工具。能力树变化 MUST 记录审计事实，并影响 run manifest 的 tool/prompt hash。

#### Scenario: 卸载插件
- **WHEN** 一个提供 `keel.media.*` CLI 的插件被卸载
- **THEN** 设置页不再显示其命令/Provider 能力；引用它的 Agent/Team 显示不可用并给出修复，
  不在运行时静默回退到其它厂商
