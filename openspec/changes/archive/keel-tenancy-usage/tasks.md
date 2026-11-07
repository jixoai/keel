# Tasks: keel-tenancy-usage

## 1. tenant-config

- [x] schema：`tenants.config jsonb` + `config_revision bigint`（默认 `{}` / 1，幂等迁移）
- [x] `internal/store/tenants.go`：`GetTenantConfig` / `SetTenantConfig`（原子 revision+1）/
      `GetTenantRevision`；未命中 `ErrNoTenant`
- [x] `internal/host/config.go`：`TenantConfig` DTO（agent/skills/mcp/hooks/compaction/telemetry/tools）
- [x] `Merge(defaults, tenant)`：标量覆盖、切片显式替换、MCP 按名合并、深拷贝
- [x] 规范 JSON 保留 nil/空切片差异（sections/roots/commands/allow 无 omitempty），
      显式清空经 PUT→GET 仍是 `[]`
- [x] `Validate()`：protocol/baseUrl/thinking/sections/skills/mcp/hooks/compaction/
      telemetry/tools 逐字段校验；section/MCP 名限 `[a-z0-9_-]+`；
      apiKeyRef 与 MCP env/headers 只允许 `${NAME}`/`env:NAME`/`secret:NAME`；
      未知 JSON 字段拒绝
- [x] `internal/httpapi/admin.go`：`PUT/GET /v1/tenants/{id}/config`，管理员 Bearer
      （scheme 大小写不敏感）+ `KEEL_ADMIN_KEY` 常量时间比较；未配置 503；缺租户 404；
      body 超限 413
- [x] 测试：revision 生命周期、缺租户、Merge 覆盖/别名隔离、Validate 正反例、
      空切片 PUT→GET 往返、secret 引用拒绝、API 鉴权与错误码

## 2. usage-ledger

- [x] schema/store 契约（本 change 无生产写入者）：`usage_ledger`（主键
      tenant/session/request_id；seq 审计列；五类 token + 非负 CHECK；
      status settled/unaccounted；created_at）；旧 seq 主键幂等迁移
- [x] `internal/store/usage.go`：`AppendUsage`（同 request_id 幂等；拒绝负 token；
      unaccounted→settled 原位升级；返回 recorded）
- [x] `internal/store/usage.go`：`SumUsage`（tenant/session/provider/model 过滤与分组，
      含 unaccounted 条目数）
- [x] `agentcore.Usage`：`CacheReadTokens` / `CacheWriteTokens` / `ReasoningTokens`（omitempty）
- [x] OpenAI Chat Completions 解码：input 减去 cached/cache_write 子集；
      `prompt_tokens_details.cached_tokens`/`cache_write_tokens`、
      `completion_tokens_details.reasoning_tokens`
- [x] Responses API 解码：input 减去 cached/cache_write；`input_tokens_details`/
      `output_tokens_details` 映射
- [x] Anthropic 解码：`input_tokens` 不含 cache，保持原值；映射 cache_read/creation
- [x] compaction 上下文 token 估算按互斥计数 input+output+cacheRead+cacheWrite
- [x] `GET /v1/tenants/{id}/usage`：分组行 + 租户总计 + unaccountedEntries（无价格）
- [x] 测试：同 seq 双 request_id、同 request_id 重放、unaccounted 升级、负值拒绝、
      聚合/过滤、跨租户隔离、OpenAI/Responses 缓存子集不重复计数与 input 钳 0、
      Anthropic 不归一化、API 输出

## 3. provider-telemetry

- [x] `provider.StreamConfig.Telemetry telemetry.Context`（仅字段）
- [x] `internal/agent/provider_telemetry.go`：`NewObservedStream(base, telemetry)`
- [x] span 在调用 base 前开始（建流失败也有 span）；StartSpan 拒绝 callback 时关闭 stream
- [x] span `pi.ai.request`：provider/model/api/first_chunk/stop/usage.input/output/
      cache_read/cache_write；不记录正文与 response id
- [x] metrics：`pi.ai.request.duration`、`pi.ai.first_chunk.duration`、`pi.ai.tokens`
- [x] 状态语义：error stop → error；aborted → ok；无 terminal 异常关闭 → error；
      nil telemetry → 原样返回
- [x] 测试：memory adapter 断言 span 名/属性/状态、metric 名与值、错误/中止/异常关闭

## 4. 验收

- [x] `gofmt` / `go vet ./...`
- [x] `KEEL_TEST_DSN=... go test -race ./internal/store ./internal/host ./internal/agent ./internal/provider`
- [x] 全仓 `go test ./...` 无回归
- [x] 本 worktree 提交（不 push）；Runner 接线留给后续 change

## 5. 后续（不在本 change，由主代理的 Runner 升级 change 承接）

本 change 只交付 schema/store 契约，不包含生产调用者；以下工作流由后续 change 交付：
- Runner `RunSpec` 接线：加载租户配置 → `Merge` + `Validate` → `host.New`
- 每个 provider 请求发前生成 request_id（UUIDv4）并落请求元数据事件；重试/重放复用
- 每次 provider 请求完成时以同一 request_id 调用 `AppendUsage`
- 崩溃恢复扫描：对无 settled 行的已启动请求补 `unaccounted`，恢复后原位升级
- `NewObservedStream` 在 composition root 包装真实 provider
