# tenant-config

## ADDED Requirements

### Requirement: 持久化租户配置与修订号

系统 MUST 在 `tenants` 行上保存租户配置文档（`config jsonb`，默认 `{}`）与
`config_revision bigint`（默认 1）。每次接受的写入 MUST 在同一语句内把 revision
加一，并返回新值；未命中租户 MUST 返回 `ErrNoTenant`。配置中只存密钥引用，
MUST NOT 存任何明文密钥或价格。

#### Scenario: 首次读取与写入
- **WHEN** 新建租户后读取配置
- **THEN** 返回 `{}` 且 revision = 1
- **WHEN** 写入一份合法配置
- **THEN** 返回 revision = 2，再读得到同一文档与 revision = 2

### Requirement: 配置 DTO 与合并语义

系统 SHALL 提供 `TenantConfig` DTO（agent/skills/mcp/hooks/compaction/telemetry/tools，
字段覆盖 `docs/HOST.md §2`）与纯函数 `Merge(defaults, tenant)`。标量与 `*bool` MUST
仅在租户显式设置时覆盖；切片 MUST 在租户非 nil 时替换（空切片表示清空）；
`mcp.servers` MUST 按名字合并；结果 MUST NOT 与任一输入共享可变数据。
本节 MUST NOT 连接 MCP、加载 skills 或读取密钥。

#### Scenario: 租户覆盖与 MCP 合并
- **WHEN** 默认配置含 model=gpt-5 与 filesystem server，租户只改 model 并覆盖 filesystem
- **THEN** 合并结果保留默认 provider/其余 server，使用租户 model 与 filesystem 参数

#### Scenario: 显式清空
- **WHEN** 租户配置 `sections: []`，默认配置有 section
- **THEN** 合并结果没有 section（显式空切片清空，nil 才继承）

### Requirement: 规范 JSON 保留空集合语义

配置文档的规范 JSON MUST 保留 nil 与空切片的区别：sections/roots/commands/allow
不带 omitempty（nil→null、[]→[]），否则“显式清空”会被下一轮读出误判为“继承”。

#### Scenario: PUT 空数组后 GET
- **WHEN** 管理员 PUT `{"agent":{"sections":[]},"skills":{"roots":[]}}` 后 GET
- **THEN** 返回的 config 中 `sections` 与 `roots` 是空数组，不是 null、不是缺省
- **AND** 与默认配置合并后仍为空

### Requirement: 配置校验

`Validate()` MUST 拒绝非法 protocol/baseUrl/thinking/重复 section/未知 fileReadTool/
空 skill root/非法 MCP 传输/hook 缺 event 或 command/阈值越界/未知 telemetry adapter/
非法 tools.allow，并指出字段路径。解析 MUST 拒绝未知 JSON 字段与非对象文档。
section 名与 MCP server 名 MUST 匹配 `[a-z0-9_-]+`，防止 system prompt 结构注入。

#### Scenario: 非法配置拒绝
- **WHEN** 提交 `agent.protocol=grpc`、`compaction.threshold=2` 或 section 名 `Bad Name`
- **THEN** `Validate` 返回带字段路径的错误

### Requirement: 密钥引用校验

`agent.apiKeyRef` 与 MCP server 的 `env`/`headers` 值 MUST 是 secret 引用
（`${NAME}` / `env:NAME` / `secret:NAME`）；明文、`Bearer <token>` 与含 userinfo 的
URL MUST 被拒绝，确保密钥不落入租户配置文档。

#### Scenario: 明文密钥被拒绝
- **WHEN** 配置 `apiKeyRef` 为明文或 header `Authorization` 为 `Bearer x`
- **THEN** `Validate` 返回错误，文档不会被写入

#### Scenario: 合法引用通过
- **WHEN** 配置 `apiKeyRef="env:OPENAI_API_KEY"`、header `Authorization="${MCP_TOKEN}"`
- **THEN** 校验通过

### Requirement: 管理员配置 API

系统 SHALL 提供 `PUT /v1/tenants/{id}/config` 与 `GET /v1/tenants/{id}/config`。
两者 MUST 要求 `Authorization: Bearer <KEEL_ADMIN_KEY>`（scheme 大小写不敏感），
密钥比较 MUST 常量时间；未配置管理员密钥时 MUST 返回 503 而非放行。
PUT 的 body MUST 是 TenantConfig 对象本身；非法文档 MUST 返回 400，body 超过
`MaxBody` MUST 返回 413，租户不存在 MUST 返回 404。

#### Scenario: 鉴权失败
- **WHEN** 未携带或携带错误 Bearer token
- **THEN** 返回 401
- **WHEN** KEEL_ADMIN_KEY 未配置
- **THEN** 返回 503

#### Scenario: 写入并读回
- **WHEN** 管理员 PUT 合法配置后 GET
- **THEN** GET 返回同一文档与新 revision
