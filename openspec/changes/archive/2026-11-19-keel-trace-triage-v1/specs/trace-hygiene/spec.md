# trace-hygiene Specification

## ADDED Requirements

### Requirement: 客户端日志与错误不得携带凭据

客户端 MUST NOT 把 Provider 密钥、会话 cookie、Authorization 头或完整请求头写入控制台、
DOM 文本、URL 查询串或可持久化存储。错误消息 MUST 经过脱敏（只保留可读原因与状态码）。

#### Scenario: 请求失败的界面提示
- **WHEN** 任一 Provider 请求失败（4xx/5xx/网络错误）
- **THEN** 界面与控制台只出现状态码与脱敏后的原因
- **AND** trace 中不出现 `Authorization`、`Bearer`、`sk-` 或 cookie 原文

### Requirement: trace 异常必须可追溯到代码

每一条被 Owner 观察到的异常 MUST 在 triage 记录中映射到具体文件/行与复现步骤，MUST NOT
以「可能是环境问题」结案而不给证据。

#### Scenario: 提交 triage 结论
- **WHEN** 一条异常被标记为已定位
- **THEN** 记录含 trace 时间戳、请求/事件摘要、代码位置与复现步骤
