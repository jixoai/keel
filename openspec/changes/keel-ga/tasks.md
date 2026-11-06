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

- [ ] schema：`api_keys`（hash/prefix/tenant_id/role）、`tenant_usage`
- [ ] `internal/auth`：Bearer / X-API-Key → tenant；401 语义
- [ ] 管理员 provisioning（创建 tenant + key，返回一次明文 key）
- [ ] RLS 迁移：`FORCE ROW LEVEL SECURITY` + 非 owner 角色 + `app.tenant_id`
- [ ] 配额：并发 run / 流 / 带宽 / 存储；429/413 + Retry-After
- [ ] 用量事件：token / 图片字节 / 存储字节（无价格字段）
- [ ] 测试：跨租户 404；单租户打满不影响他人

## 3. provider-files

- [ ] `agentcore` 增加 `ImageRefContent{BlobID, Mime, Width, Height}`（或 `ImageContent.BlobID`）
- [ ] 事件只存 blobId；worker 出站前解析
- [ ] provider 能力位：`SupportsImageURL` / `SupportsFileID` / `MaxImageBytes`
- [ ] resolver：`file_id` > `url` > `base64`，流式读取
- [ ] adapter：Gemini Files（48h）→ OpenAI Responses file_id → Anthropic URL → Anthropic Files
- [ ] provider file 缓存 `(sha256, provider, workspace)` + TTL 懒刷新
- [ ] 测试：mock provider server 断言 wire 形态与内存上限

## 4. ops-runtime

- [ ] CI：build/vet/test -race/容器构建（Go 1.27.3，PG service）
- [ ] Dockerfile（distroless）+ docker-compose（PG + Keel）
- [ ] `/v1/ready`（PG + 迁移）；`/metrics`（Prometheus 文本，固定基数）
- [ ] 结构化日志 + request id
- [ ] SIGTERM 排水：`event: drain` + 速率关闭
- [ ] 部署文档（单机 2GB 推荐参数）

## 5. blob-lifecycle

- [ ] 缩略图异步生成（最长边 320），`thumb_status`
- [ ] 引用计数由事件维护（`(blob, session, eventSeq)` 幂等）
- [ ] TTL sweeper：metadata → chunks → CAS 文件，幂等
- [ ] base64 offset fuzz（0/3 倍数/chunk 边界/末尾）
- [ ] 文档：CAS 目录布局与 GC

## 6. 验收

- [ ] 1000 会话在线 + 64 并发 run，RSS < 2GB
- [ ] 随机 SIGKILL（每 30s，持续 10min）0 丢事件、0 重复最终回复
- [ ] 2GB cgroup 复跑
- [ ] 子代理交叉复核（本项目不使用 Codex）
- [ ] 报告 + 公网链接 + 归档本 change
