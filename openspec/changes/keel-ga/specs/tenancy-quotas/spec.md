# tenancy-quotas

## ADDED Requirements

### Requirement: API key 鉴权

系统 SHALL 接受 `Authorization: Bearer <key>` 或 `X-API-Key`，把 key 解析为 tenant；
数据库只存 key 的哈希与前缀。无效/缺失 key MUST 返回 401。

#### Scenario: 无效 key
- **WHEN** 请求携带未知 key
- **THEN** 返回 401，且不泄露 tenant 是否存在

### Requirement: 行级安全

启用 RLS 时，租户表 MUST 使用 `FORCE ROW LEVEL SECURITY`；应用连接 MUST 是非 owner
角色，并在事务内设置 `app.tenant_id`。跨租户查询 MUST 返回 0 行。

#### Scenario: 越权读取
- **GIVEN** RLS 已启用，连接角色非 owner
- **WHEN** 查询他租户的 session
- **THEN** 返回 0 行（对外表现为 404）

### Requirement: 每租户配额

系统 SHALL 限制每租户的：并发 run、并发流、出站带宽、存储字节。超限 MUST 快速拒绝
（429 或 413）并给出 `Retry-After`/上限说明。

#### Scenario: 单租户打满
- **WHEN** 某租户并发 run 超过配额
- **THEN** 该租户新 run 排队或 429，其他租户不受影响

### Requirement: 计量事件

系统 SHALL 记录客观用量事件（模型 token、图片字节、存储字节），MUST NOT 在 schema
中存价格。

#### Scenario: 用量可查询
- **WHEN** 管理员查询某租户当月用量
- **THEN** 返回 token/字节计数，不含金额
