# usage-ledger

## ADDED Requirements

### Requirement: request_id 幂等身份

本 change 交付 `usage_ledger` 的 schema/store 契约，不包含生产调用者。
`usage_ledger` 主键 MUST 为 `(tenant_id, session_id, request_id)`；store MUST 在该
键上幂等。一个事件 `seq` 可以派生多个请求，`seq` 只作审计列，MUST NOT 作为幂等键。
`request_id` 的生成、请求元数据事件的持久化与重试复用规则不在本 change 范围，
由后续 runner 升级 change 在接线时交付（建议每请求 UUIDv4、重试/重放复用）。

#### Scenario: 同 seq 两个请求
- **WHEN** 同一 `(tenant, session, seq)` 写入两个不同 request_id
- **THEN** 两行都记录，聚合条目数为 2

#### Scenario: 重放只记一次
- **WHEN** 同一 `(tenant, session, request_id)` 写入两次 settled
- **THEN** 第二次 recorded=false，聚合计数不增加

### Requirement: 账本字段与状态语义

账本 MUST 记录 provider、model、input/output/cache_read/cache_write/reasoning
六类计数与 `status`（`settled` / `unaccounted`），MUST NOT 包含价格或金额列。
store MUST 支持同 request_id 的重复 settled 只生效一次，并支持
`unaccounted` → `settled` 的原位升级（不新增行）。
“发请求前写请求元数据、崩溃后扫描补 unaccounted”的完整工作流不在本 change 范围，
由后续 runner 升级 change 交付；本 change 只冻结能够表达该语义的 schema/store 契约。

#### Scenario: 丢账补偿后升级
- **WHEN** 先写 `unaccounted`，再以同一 request_id 写 settled
- **THEN** 行数不变，tokens 更新为真实值，unaccounted 计数归零

### Requirement: 用量聚合查询

系统 SHALL 提供 `SumUsage`，按 `(tenant, session, provider, model)` 分组返回
条目数、unaccounted 条目数与五类 token 总计，并支持 tenant/session/provider/model
过滤；跨租户行 MUST NOT 混入结果。

#### Scenario: 分组与过滤
- **WHEN** 同一会话写入两个 gpt-5 请求与一个 gpt-5-mini 请求
- **THEN** 默认查询返回两个分组；按 model 过滤只返回匹配分组，跨租户请求不出现

### Requirement: Usage token 字段与上下文估算

`agentcore.Usage` SHALL 携带 `CacheReadTokens`、`CacheWriteTokens`、
`ReasoningTokens`（JSON omitempty），且 MUST NOT 携带价格。账本 token 计数
MUST 非负：schema 有 CHECK，store 写入前拒绝负值。compaction 的上下文
token 估算 SHALL 采用 `input + output + cacheRead + cacheWrite`；reasoning 已包含
在 output 内，MUST NOT 重复相加。

#### Scenario: 缓存计数计入上下文
- **WHEN** Usage 为 input=50/output=40/cache_read=30/cache_write=20
- **THEN** 上下文估算为 140，且 reasoning 不额外相加

### Requirement: Provider 输入归一化

四个输入侧计数 MUST 互斥。OpenAI Chat Completions 与 Responses 的
`prompt_tokens`/`input_tokens` 含缓存子集，解码器 MUST 计算
`input = max(0, raw_input - cache_read - cache_write)`，并在存在时映射
`cache_write_tokens`。Anthropic 的 `input_tokens` 本身不含 cache，MUST 原样映射，
不做减法。缺失字段 MUST 保持 0，不得猜测。

#### Scenario: OpenAI 缓存子集不重复计数
- **WHEN** `prompt_tokens=100` 且 `cached_tokens=30`、`cache_write_tokens=20`
- **THEN** Usage 为 input=50/cache_read=30/cache_write=20

#### Scenario: Anthropic 不归一化
- **WHEN** `input_tokens=10`、`cache_read_input_tokens=30`、`cache_creation_input_tokens=20`
- **THEN** Usage 为 input=10/cache_read=30/cache_write=20

### Requirement: 管理员用量 API

系统 SHALL 提供 `GET /v1/tenants/{id}/usage`，鉴权与配置 API 相同（管理员 Bearer +
常量时间比较，未配置 503）。响应 MUST 包含按 session/provider/model 分组的行与租户
总计，含 `entries` 与 `unaccountedEntries`，只含计数；租户不存在 MUST 返回 404。

#### Scenario: 查询租户用量
- **WHEN** 管理员查询已有用量记录的租户
- **THEN** 返回分组行与总计，字段为 token 计数与 unaccounted 条目数，无金额
