# Proposal: keel-tenancy-usage

> 立项：2026-11-06（租户配置 / 计量 / Provider 遥测工作流）。
> 本 change 只交付**可集成的模块与 API**：租户配置文档、用量账本、provider 请求级
> telemetry 契约与实现。Runner 的 RunSpec 接线由主代理在 Runner 升级时单独完成，
> 不在本 change 范围内。

## Why

`keel-ga` 的 `tenancy-quotas` 只定义了目标（API key → tenant、配额、计量事件），
但运行时目前没有租户级配置面、没有可查询的 token 账本，provider 请求也没有
请求级 span/token 指标。这三块不补齐，多租户只能是「共享一个默认 Agent」，
用量无法对账，模型调用的可观测性只能停在 run 粒度。

本 change 把这些能力做成**独立、可测试、无 Runner 耦合**的模块：
主代理接线时只需要调用既有函数，不用改本 change 的任何语义。

## What Changes

### 1. `tenant-config`

- `tenants.config jsonb NOT NULL DEFAULT '{}'` + `config_revision bigint NOT NULL DEFAULT 1`；
  幂等迁移，存量租户读作 `{}` / revision 1。
- `internal/store/tenants.go`：`GetTenantConfig` / `SetTenantConfig`（原子 revision+1，
  返回新值）/ `GetTenantRevision`；缺租户返回 `ErrNoTenant`。
- `internal/host/config.go`：`TenantConfig` DTO，覆盖 `docs/HOST.md §2` 的
  agent / skills / mcp / hooks / compaction / telemetry / tools 七段；
  `Merge(defaults, tenant)` 与 `Validate()` 只做解析/合并/校验——不连接 MCP、
  不加载 skills、不读密钥。section/MCP 名限 `[a-z0-9_-]+`；apiKeyRef 与 MCP
  env/headers 只接受 `${NAME}`/`env:NAME`/`secret:NAME` 引用；规范 JSON 保留
  nil（继承）与空切片（清空）的差异。
- `internal/httpapi/admin.go`：`PUT/GET /v1/tenants/{id}/config`，管理员
  `Authorization: Bearer`（scheme 大小写不敏感）与 `KEEL_ADMIN_KEY` 常量时间比较；
  未配置 key 时 503（不裸奔）；body 超限 413。

### 2. `usage-ledger`

- `usage_ledger` 表：主键 `(tenant_id, session_id, request_id)`。`request_id` 由后续
  Runner 接线 change 在发请求前生成并写入请求元数据事件（重试/重放复用）；一个事件
  seq 派生的多个请求（主回合、compaction summary、重试）各占一行，`seq` 仅作审计列。
  记录 provider/model 与 input/output/cache_read/cache_write/reasoning 五类 token，
  **没有价格列**；`status` 区分 `settled` 与 `unaccounted`。
- 崩溃补偿语义：provider 已计费但账本未落库的请求以零 token 的 `unaccounted` 行保留，
  恢复拿到真实 usage 后在同一行升级为 `settled`，既不丢账也不双计。本 change 只交付
  可表达该语义的 schema/store 契约，实际扫描与补偿工作流由 Runner 接线 change 交付。
- `internal/store/usage.go`：`AppendUsage`（同 request_id 幂等、unaccounted→settled
  原位升级、拒绝负 token、返回 recorded）/ `SumUsage`（按 tenant/session/provider/model
  聚合，含 unaccounted 条目数）。
- `GET /v1/tenants/{id}/usage`：返回分组行与租户总计，只有计数。
- `agentcore.Usage` 增加 `CacheReadTokens` / `CacheWriteTokens` / `ReasoningTokens`
  （`omitempty`）；OpenAI Chat/Responses 解码器把 input 归一化为互斥余量
  `max(0, raw_input - cache_read - cache_write)` 并映射 cache_write（存在时），
  Anthropic 的 input 本身不含 cache 保持原值；缺失字段保持 0。compaction 上下文 token
  估算按互斥计数 `input + output + cacheRead + cacheWrite`。

### 3. `provider-telemetry`

- `provider.StreamConfig` 增加 `Telemetry telemetry.Context` 字段（接缝，不强制使用）。
- `internal/agent/provider_telemetry.go`：`NewObservedStream(base, telemetry)` 包装
  `StreamFn`，记录 `pi.ai.request` span（provider/model/api/first_chunk/stop/
  usage.input/output/cache_read/cache_write）与
  `pi.ai.request.duration`、`pi.ai.first_chunk.duration`、`pi.ai.tokens` 指标；
  不记录消息正文、response id 或任何密钥。
- 错误 stop 置 span error；aborted 保持 ok；无 terminal 的异常关闭置 error。
  telemetry 为 nil 时原样返回 base，零开销。

## Impact

- 表：`tenants` 增两列；新增 `usage_ledger`（`schema.sql` 幂等）。
- 新文件：`internal/store/{tenants,usage}.go`、`internal/host/config.go`、
  `internal/httpapi/admin.go`、`internal/agent/provider_telemetry.go` 及测试。
- 文档：`docs/HOST.md` 增加 `KEEL_ADMIN_KEY`、管理员配置/用量 API、secret 引用规则。
- 兼容：全部为新增字段/路由；现有 API、事件与 Runner 行为不变。
- 依赖：无新增外部依赖。

## Non-Goals

- 不改 `internal/agent/runner.go`、`internal/loop/**`；Runner 接线由主代理完成。
- 不做 durable/task、MCP OAuth、Prometheus/OTLP adapter、CI。
- 不做价格计算、配额执行、RLS、API key 管理（`keel-ga` 的其它条目）。
