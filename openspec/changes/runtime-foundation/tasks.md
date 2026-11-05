# Tasks: runtime-foundation

## 0. 仓与规格

- [x] 建仓 `jixoai/fde2e-agent-runtime`（私有）＋ README/AGENTS/docs
- [x] `go.mod` 固定 Go **1.27.3**
- [x] OpenSpec 规格落地（runtime-core / stream / blob / backpressure / tenant / provider）
- [ ] 主仓挂 submodule ＋ PROJECT-MAP 登记

## 1. 搬运 pigo（MIT，pin `891d1f37`）

- [x] 记录 pin commit 到 `docs/UPSTREAM.md`
- [x] `internal/agentcore`（类型原样；MIT 头）
- [x] `internal/provider`（协议/流式；去掉 CLI config 依赖，内联 `GenericBaseURLEnvVar`）
- [x] `internal/loop`（runtime/loop + stream_response + telemetry；去掉 reminders 与文件 checkpoint，改回调）
- [x] `internal/agenttool`（registry/executor/batch/retry）
- [x] `internal/compaction`（cutpoint/compact/tokens/summary）
- [x] 编译 + `go vet` 通过
- [ ] 为 loop 补单测（fake stream：文本/工具/压缩/取消）

## 2. 无状态核心（runtime-core）

- [x] PG schema：`tenants` / `sessions` / `events` / `checkpoints` / `submissions` / `tasks`
- [x] 事件 append（每会话单调 seq，会话行锁分配）
- [x] checkpoint 写入（投影，不覆盖事件）
- [x] `requestId` 唯一约束（跨进程幂等）
- [x] E2E：SIGKILL → 重启 → 同一 submission / 单条事件
- [x] E2E：100 会话 × 20 事件，SIGKILL 后 seq 连续、无缺口
- [ ] loop 的持久任务接线（工具执行/压缩 `resume()`）

## 3. 流式（runtime-stream）

- [x] SSE：`Last-Event-ID`/`after` 游标、15s 心跳、5s 兜底
- [x] PG `LISTEN/NOTIFY` 唤醒 + 本地 hub（丢通知只延迟一个心跳）
- [x] 断线重连 E2E：重启后从游标继续，首帧 = 游标+1
- [x] 慢客户端护栏（4s 单次写 + 15s 窗口 12KB/s 两级）
- [x] 每写 deadline，SSE 不设全局 `WriteTimeout`

## 4. Blob（blob-store）

- [x] `POST /v1/files`：流式 spool（临时文件）+ sha256 + 租户内去重
- [x] PG 64KiB 分块 / >1MiB 本地 CAS（sha256 两级目录）
- [x] `GET /v1/files/{id}?format=base64`：流式编码，支持 wire offset 续传
- [x] `GET /v1/files/{id}`：原始流 + `Range`
- [x] `ref_count` / `expires_at` / `ReleaseExpired`（sweeper 待接线）
- [x] E2E：1MiB+3 上传 → 全量 base64 → SIGKILL → offset 续传一致
- [ ] 缩略图异步生成（可选）

## 5. 反压（runtime-backpressure）

- [x] 全局 token bucket（`-bandwidth`，burst ≥ 2×87KB）
- [x] 三类分池 + Large 保护位（小请求不能吃光大请求余量）
- [x] SSE 控制流独立准入（只受总 cap 约束）
- [x] 有界 worker（队列满 429/503 + `Retry-After`）
- [x] 慢客户端取消 + offset 记录（客户端上报）
- [x] 客户端 full jitter 重连（loadtest 实现）
- [x] 压测：1000 会话 / 1MiB 图 / 120s（RSS 437MB，p50 14.1s，0 SSE 断开）

## 6. 多租户（tenant-isolation）

- [x] `tenant_id` 全表 + 复合索引
- [ ] RLS 可选启用 + 非 owner 角色
- [ ] 每租户配额（并发/带宽/存储）
- [x] 测试：所有查询带 `tenant_id`（越权读取返回 404）

## 7. 端到端验收

- [x] 1000 会话 × 周期图片压测（RSS <2GB，0 OOM）
- [x] `kill -9` 注入 + 重启恢复（4/4 E2E）
- [x] RSS 曲线、p50/p95/p99、吞吐
- [ ] HTML 报告（`.agents/reports/runtime/<date>-agent-runtime-2gb/`）＋ 公网链接
- [ ] 子代理交叉复核（本项目不使用 Codex）

## 8. 已知边界（诚实登记）

- 2GB 是 `GOMEMLIMIT` 软限 + 实测 RSS（437MB）；未在 Linux 2GB cgroup 内复跑
  （Docker Desktop 端口转发在 1000 连接下不稳定）。
- loadtest 的图片延迟由**客户端限速**主导（100KB/s/流），不是服务端处理时间。
- Agent loop 已搬运但尚未接到事件日志；当前 HTTP 层验证的是存储/流式/反压/无状态契约。
- PG 分块路径在 1MiB 图上验证；>1MiB 走 CAS，CAS 读路径已有单测。
