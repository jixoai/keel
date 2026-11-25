# pi-hooks

## ADDED Requirements

### Requirement: 租户 hooks 进入出货执行路径

系统 SHALL 从租户文档 `hooks.commands` 构建 `internal/hooks.Registry`，并把 registry
传给 `agent.Runner`；`go list -deps ./cmd/keel` MUST 包含 `internal/hooks`。
命令钩子 MUST 按 PI 事件名分发；未识别的 event/command MUST 在装配期报错。

#### Scenario: 租户 deny 钩子阻止工具
- **WHEN** 租户配置 `tool_call` 命令钩子返回 `deny`
- **THEN** 模型发起的该工具调用不执行，工具结果包含拒绝原因，run 继续

#### Scenario: 钩子超时不挂 run
- **WHEN** 命令钩子超过配置 timeout
- **THEN** 按 failure policy 处理（默认 continue），run 不因超时永久挂起

### Requirement: 工具参数/结果可修改

`tool_call` 的 modify SHALL 替换后续钩子与执行器看到的参数；`tool_result` 的 modify
SHALL 按字段替换模型看到的工具结果，且不得把钩子正文写入模型上下文之外的日志。

#### Scenario: 参数改写
- **WHEN** `tool_call` 钩子返回新的 input JSON
- **THEN** 工具执行器收到改写后的参数

### Requirement: 压缩前钩子可取消

`session_before_compact` SHALL 在自动压缩前执行；deny/cancel MUST 跳过本次压缩并保留
原上下文，run 继续。

#### Scenario: 取消自动压缩
- **WHEN** 上下文超过阈值且 `session_before_compact` 返回 deny
- **THEN** 不产生 compaction 事件/checkpoint，原上下文继续服务请求

### Requirement: 钩子审计

每次钩子执行 SHALL 产生一条结构化审计记录，包含 hook/event/decision/duration/error；
MUST NOT 包含消息正文、工具参数或密钥。当前阶段审计写结构化日志，不新增客户端可见事件 kind。

#### Scenario: 审计脱敏
- **WHEN** 钩子拒绝一个含敏感参数的工具调用
- **THEN** 审计记录只有 hook/event/decision/原因，不含参数正文
