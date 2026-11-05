# tenant-isolation

## ADDED Requirements

### Requirement: 单库逻辑隔离

系统 SHALL 在单 PostgreSQL 库内以 `tenant_id` 隔离数据；所有租户表 MUST 有以
`tenant_id` 打头的复合索引。MUST NOT 使用 schema-per-tenant 或 database-per-tenant。

#### Scenario: 查询必带租户
- **WHEN** 任意查询访问租户数据
- **THEN** 其谓词包含 `tenant_id`，执行计划使用复合索引

### Requirement: 行级安全（可选启用）

系统 SHALL 支持 PostgreSQL RLS（`FORCE ROW LEVEL SECURITY` + 非 owner 连接角色），
启用后跨租户读取 MUST 返回零行。

#### Scenario: 越权读取
- **GIVEN** RLS 已启用，会话角色非 owner
- **WHEN** 查询他租户的行
- **THEN** 返回 0 行

### Requirement: 每租户配额

系统 SHALL 支持每租户的并发连接数、带宽、存储配额；超限 MUST 在应用层拒绝（429/413），
并 SHALL 有数据库层兜底（连接池/大小限制）。

#### Scenario: 单租户打满
- **WHEN** 某租户并发超过配额
- **THEN** 该租户的新请求被限流，其他租户不受影响
