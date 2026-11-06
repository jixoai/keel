# Proposal: keel-ga

> 立项：Keel 首版实测通过后（2026-11-05），把「能跑的底座」推进到「能承载真实 Agent 的 GA 底座」。
> 本 change 覆盖首版 `runtime-foundation` 未完成的运行时核心能力（Agent 执行、租户、provider 文件、
> 运维、blob 生命周期）；完成即归档。
> 生态层（可观测/可恢复/压缩/Skills/MCP/Hooks）见 `keel-ecosystem`；两者按 `docs/MATURITY.md`
> 的 G0–G5 闸门验收。Agent 执行部分已于 2026-11-06 完成。

## Why

首版证明了最难的三件事——1000 个带图片会话在 2GB 内、SSE 断线续传、4/4 跨 SIGKILL——
但 HTTP 层目前只验证了存储/流式/反压/无状态契约：**Agent loop 尚未接到事件日志**，
没有真实的多租户配额/鉴权，没有 provider 文件协议，也没有可部署的 CI/镜像/指标。
这些缺口不补，Keel 只能当「文件+SSE 服务」，不能当 Agent 底座。

## What Changes

### 1. `agent-execution`（核心）

- 事件溯源的消息模型：`user.message` / `assistant.message` / `tool.result` / `compaction`
  是唯一真值；worker 从事件重建上下文。
- 持久 run：`runs` 表 + 租约 + `FOR UPDATE SKIP LOCKED`；进程重启后过期租约被回收。
- 工具执行：内置 `clock` / `echo`，工具结果落事件；未完成工具调用在恢复时重新执行
  （at-least-once，文档明确要求工具幂等或带幂等键）。
- 压缩：`compaction` 事件追加，checkpoint 写 `checkpoints`，不覆盖历史。
- 流式：首版只持久化完整消息（提交即可见）；token 级 partial 作为**非持久**旁路，
  仅在单进程内广播（后续可替换为 Redis/广播层）。

### 2. `tenancy-quotas`

- API key → tenant 解析（`Authorization: Bearer` / `X-API-Key`），key 只存哈希。
- RLS：`FORCE ROW LEVEL SECURITY` + 非 owner 连接角色；应用连接设置 `app.tenant_id`。
- 每租户配额：并发 run、并发流、带宽、存储字节；超限 429/413。
- 租户 provisioning API（管理员 key）。

### 3. `provider-files`

- 消息里的图片只存 `blobId`；出站时按 provider 能力选 `file_id` / `url` / `base64`。
- 能力位：`SupportsImageURL` / `SupportsFileID` / `MaxImageBytes`。
- adapter 顺序：Gemini Files → OpenAI Responses `file_id` → Anthropic URL → Anthropic Files →
  DashScope/OpenRouter URL；base64 永远兜底。
- provider file 缓存键 `(sha256, provider, workspace)`，带 TTL 懒刷新。

### 4. `ops-runtime`

- CI：`go vet`、`go test -race`、构建、容器镜像；Go 1.27.3 固定。
- 部署：Dockerfile（distroless）、docker-compose（PG + Keel）、健康/就绪探针。
- 指标：Prometheus 文本格式（流/队列/PG/运行时），固定基数。
- 排水：发布时先发 `event: drain` + `retry_after_ms`，再按速率关闭 SSE。
- 结构化日志 + 请求 ID。

### 5. `blob-lifecycle`

- 缩略图异步生成（最长边 320px），原图不变。
- `ref_count` 由事件引用维护；TTL sweeper 回收无引用 blob 与 CAS 文件。
- Range/offset 续传的模糊测试（4 字符 ≡ 3 字节边界）。

## Impact

- 新表：`runs`、`api_keys`、`tenant_usage`；`blobs` 增 `thumb_status`。
- 新包：`internal/agent`（事件↔消息、runner、worker）、`internal/auth`、`internal/metrics`。
- 服务端新增 endpoint：`POST /v1/sessions/{id}/run`（显式触发，可选）、`GET /metrics`。
- 不破坏首版 HTTP 契约；`/v1/sessions/{id}/input` 继续幂等提交，并自动入队 run。
- 兼容：无 provider key 时使用 `fake` provider（确定性回复），便于本地与 E2E。
- 风险：1000 个并发模型 run 会突破 2GB；用有界 run worker（默认 64）+ 队列；
  压测目标改为「1000 会话在线 + 64 并发 run」。
