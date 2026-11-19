> **状态（2026-11-08 对账）**：本 change 是历史任务板，**已冻结**。剩余未完成项不再单独推进，
> 已并入 `keel-service-graph`（其 `tasks.md` 是当前唯一在推进的板：W0 基线/R1 差分、
> W1 契约内核、W2 服务图与 loadout、W3 PI loop 与 conformance 门、W4 handoff、
> W5 外部网关、W6 集成与验收）。本板保留原文，仅作历史对照；后续以
> `keel-service-graph` 为准，勿在此勾选。
>
> **2026-11-15 重激活**：service-graph 已归档（89/91）；按 `docs/RESIDUAL-PLAN.md` §1/§2 的
> 去重归并，**§2 tenancy-quotas 与 §3 provider-files 由本板履行并恢复勾选**（其余条目
> ——§1 剩余、§4–§6——维持历史对照，去向以 RESIDUAL-PLAN 各 Wave 为准）。

# Tasks: keel-ga

## 1. agent-execution（核心，先做）

- [x] schema：`runs`（tenant/session 主键、status、lease_until、attempts、cursor_seq、last_error）
- [x] store：`EnqueueRun` / `ClaimRun`（`FOR UPDATE SKIP LOCKED`）/ `RenewLease` / `FinishRun`
- [x] `internal/agent`：事件 → `agentcore.MessageList` 重建（user/assistant/toolResult/compaction）
- [x] `internal/agent`：Runner 消费 loop 事件，按 `message_end` / `tool_execution_end` / `compaction` 落事件
- [x] 恢复：最后一个 assistant 带未完成 toolCall 时先补执行工具，再继续 loop
- [x] Worker：有界并发（默认 64）、轮询 200ms、租约 30s + 10s 续租
- [x] 内置工具：`clock`、`echo`（JSON Schema）
- [x] fake provider：无凭据时确定性回复；重复提交不重复回复
- [x] `/v1/sessions/{id}/input` 提交后自动入队 run
- [x] E2E：输入 → 回复事件；kill 在模型流中 → 重启后恰好一条最终回复
- [x] E2E：kill 在工具执行中 → 恢复后一条 tool.result（at-least-once）
- [ ] 压缩：超阈值追加 `compaction` 事件 + checkpoint，历史可回放
- [ ] 文档：at-least-once 工具契约、单会话串行、partial 非持久

## 2. tenancy-quotas

- [x] schema：`api_keys`（hash/prefix/tenant_id/role）、`tenant_usage`
      （`internal/store/keys.go`、`byte_usage.go`、`schema.sql`；用量无价格字段）
- [x] `internal/auth`：Bearer / X-API-Key → tenant；401 语义
      （`internal/auth/keys.go`：`keel_<role>_` 前缀、未知/撤销一律 401 不区分、非 key 流量原样透传）
- [x] 管理员 provisioning（创建 tenant + key，返回一次明文 key）
      （`cmd/keel/admin.go` 直连 DB 自举 + `POST /v1/admin/tenants`；`TestAdminTenantCreatePrintsKeyOnce`）
- [x] RLS 迁移：`FORCE ROW LEVEL SECURITY` + 非 owner 角色 + `app.tenant_id`
      （`internal/store/rls.go` 双角色：operator 池 `app.bypass_rls='on'`、tenant 池 `SET ROLE keel_app` +
      `'off'`；`SET LOCAL` 不跨连接泄漏；19 张租户表；幂等 + advisory lock）
- [x] 配额：并发 run / 流 / 带宽 / 存储；429/413 + Retry-After
      （`internal/quota/edge.go`：run 沿用 kernel 配额；流/存储硬拒，带宽为整形不拒绝——见记账）
- [x] 用量事件：token / 图片字节 / 存储字节（无价格字段）
      （token 复用 `usage_ledger`；字节走 `tenant_usage`，`CompleteBlob` 同事务记账）
- [x] 测试：跨租户 404；单租户打满不影响他人
      （`TestRLSCrossTenantReadsReturnNoRows`/`TestRLSBlocksCrossTenantWrite`/
      `TestCrossTenantSessionIs404`/`TestEdgeStreamQuotaIsPerTenant`；主代理独立复跑 `go test -race` 5 包全绿）

> **记账（2026-11-15，主代理审查）**：① RLS 目前只覆盖会话列表这一个请求路径（`sessions_pg.go`），
> 其余请求路径仍走 operator 池——「第二道防线」尚未全路径生效，留 W2/W3 收口；
> ② 带宽配额按整形实现而非 429（取整形语义，已在代码注释说明）；
> ③ chunked 上传无 `Content-Length` 时不做存储预检，只在 `CompleteBlob` 计量（流式中断需改
> blob.Uploader，留待后续）；④ `internal/httpapi`（`cmd/runtime` 单租户路径）未承载租户配额。

## 3. provider-files

- [x] `agentcore` 增加 `ImageRefContent{BlobID, Mime, Width, Height}`（或 `ImageContent.BlobID`）
      （对账：`internal/agentcore/content.go` 新增 `ContentTypeImageRef`/`ImageRefContent`
      （BlobID/MimeType/Size/Width/Height/Name）+ `decodeContent` 分支；wire 形态 `ImageContent`
      扩出 `FileID`/`URL`，`WireForm()` 自述形态）
- [x] 事件只存 blobId；worker 出站前解析
      （对账：`internal/agent/messages.go` 把 user.message `attachments[{fileId,mime:image/*}]`
      解成 `ImageRefContent`（base64 永不入事件）；`internal/agent/images.go` 在 runner 出站前
      逐 block 替换为 wire 形态，失败降级为具名文本提示 + warn，不炸 run）
- [x] provider 能力位：`SupportsImageURL` / `SupportsFileID` / `MaxImageBytes`
      （对账：`internal/provider/imagecaps.go` `ImageCaps{SupportsFileID,SupportsImageURL,
      MaxImageBytes,MaxInlineBytes}` + `ImageCapsForAPI(apiType)` 按协议声明；`internal/agent/model.go`
      `ActiveModel.ImageCaps`，`internal/servicehost/models.go` 解析时填充）
- [x] resolver：`file_id` > `url` > `base64`，流式读取
      （对账：`internal/provider/imagefiles.go` `ImageResolver.Resolve` 固定降级序 + 逐级记录降级理由；
      base64 只分配 `WireLen(size)` 单缓冲，upload 走 `io.Pipe` 流式；`internal/blob/image_source.go`
      `Info`/`Open`/`EncodeBase64` 三面）
- [x] adapter：Gemini Files（48h）→ OpenAI Responses file_id → Anthropic URL → Anthropic Files
      （对账：`internal/provider/imageupload.go` `GeminiFileUploader`（resumable 两跳，48h TTL）、
      `OpenAIFileUploader`（multipart purpose user_data → Responses `file_id`）、
      `AnthropicFileUploader`（beta 头）、`PathURLSigner`（url 形态）；`internal/provider/providers.go`
      anthropic 编码器发 base64/url/file 三形态，`internal/provider/responses.go` 发 file_id/image_url）
- [x] provider file 缓存 `(sha256, provider, workspace)` + TTL 懒刷新
      （对账：`internal/provider/imagefiles.go` `FileCache.GetOrUpload`，key 为 sha256+provider+workspace
      摘要，过期 handle 下次读取时重传（Gemini 48h），超界按最近过期淘汰）
- [x] 测试：mock provider server 断言 wire 形态与内存上限
      （对账：`internal/provider/imagewire_test.go` 三形态 × 各 adapter（含 mock server 端到端断言
      file_id/url/data-URI）；`internal/provider/imagefiles_test.go` 降级序、TTL 刷新、内容摘要去重、
      超限拒绝、base64 单缓冲与三厂商上传 wire；`internal/servicehost/images_test.go`
      真 Boot 装配 → 真 resolver → mock vendor 断言；未接线时 nil resolver 降级有测）

## 4. ops-runtime

- [x] CI：build/vet/test -race/容器构建（Go 1.27.3，PG service）
- [x] Dockerfile（distroless）+ docker-compose（PG + Keel）
- [x] `/v1/ready`（PG + 迁移）；`/metrics`（Prometheus 文本，固定基数）
- [x] 结构化日志 + request id
- [x] SIGTERM 排水：`event: drain` + 速率关闭
- [x] 部署文档（单机 2GB 推荐参数）

## 5. blob-lifecycle

- [x] 缩略图异步生成（最长边 320），`thumb_status`
      （`internal/blob/thumb.go`；store `blob_lifecycle.go` 状态机；路由
      `GET /v1/files/{id}/thumbnail`；证据 `evidence/blob-lifecycle/`）
- [x] 引用计数由事件维护（`(blob, session, eventSeq)` 幂等）
      （`internal/kernel/blobrefs.go` + `internal/store/blob_refs.go`，在 append 事务内投影）
- [x] TTL sweeper：metadata → chunks → CAS 文件，幂等
      （`internal/store/blob_lifecycle.go` + `internal/blob/gc.go`；`keel serve`/runtime 周期接线）
- [x] base64 offset fuzz（0/3 倍数/chunk 边界/末尾）
      （`internal/blob/base64_offsets_test.go`：表驱动 + 1000 随机 4 倍数 offset + fuzz 属性）
- [x] 文档：CAS 目录布局与 GC
      （`docs/BLOB-LIFECYCLE.md`）

## 6. 验收

- [ ] 1000 会话在线 + 64 并发 run，RSS < 2GB
- [ ] 随机 SIGKILL（每 30s，持续 10min）0 丢事件、0 重复最终回复
- [ ] 2GB cgroup 复跑
- [ ] 子代理交叉复核（本项目不使用 Codex）
- [ ] 报告 + 公网链接 + 归档本 change
