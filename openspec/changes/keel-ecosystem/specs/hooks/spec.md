# hooks

## ADDED Requirements

### Requirement: 事件模型

系统 SHALL 在 `turn.start`、`turn.end`、`tool.before`、`tool.after`、`run.stop`、
`compaction.before`、`compaction.after` 暴露 hook 点。

#### Scenario: 工具前
- **WHEN** 模型请求调用工具
- **THEN** `tool.before` 在该工具执行前被调用

### Requirement: 两种实现

hook SHALL 支持进程内 Go 实现与命令钩子（JSON stdin/stdout）；
命令钩子 MUST 有超时（默认 5s）与失败策略。

#### Scenario: 命令钩子超时
- **WHEN** 命令钩子超过 5s 未返回
- **THEN** 按配置的策略继续或拒绝，run 不挂死

### Requirement: 决策语义

hook 返回 `allow | deny | modify`；`deny` MUST 阻止工具执行并产生审计；
`modify` MUST 只影响本次调用的参数。

#### Scenario: 拒绝危险命令
- **WHEN** `tool.before` 对 bash 工具返回 deny
- **THEN** 工具不执行，模型收到拒绝结果

### Requirement: 审计

每次 hook 执行 SHALL 追加 `hook.executed` 事件（hook 名、耗时、决策）；
该事件 MUST NOT 进入模型上下文。

#### Scenario: 事后追溯
- **WHEN** 查询会话事件
- **THEN** 能看到哪次工具调用被哪个 hook 拒绝

### Requirement: 观察型失败不阻断

标记为 observer 的 hook 失败 MUST 只记日志，不影响 run。

#### Scenario: 指标 hook 报错
- **WHEN** 观察型 hook 返回错误
- **THEN** run 继续，日志记录错误
