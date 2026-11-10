# Keel · 架构与决策

> 本文件记录底座的技术判断与**已验证的量化依据**。数字来自 2026-11-05 的实测
> （见 `docs/benchmarks/`，原始报告在 `.agents/reports/`）；未验证的推断都标「待验证」。

## 1. 目标负载（Owner 硬指标）

| 维度 | 目标 |
|---|---|
| 进程内存上限 | **2GB**（Go RSS，含所有租户） |
| 租户数 | 100–200（**逻辑隔离**，不是 200 个进程） |
| 每租户并发 | 5–10 个会话 |
| 总会话 | **1000 个持续会话**（每个都可能带图片） |
| 流式 | 图片：存储 → base64 → HTTP 全程流式；单流缓冲 ≤64KB |
| 无状态 | `kill -9` 后重启，会话继续、不丢不重（SSE `Last-Event-ID`/offset 续传） |

## 2. 进程模型：单进程、无状态、共享存储

```
                    ┌──────────────────────────────────────┐
   HTTP/SSE  ──────▶│  keel (Go, 1 process)  │
   (1000 conn)      │  ┌─────────┐ ┌─────────┐ ┌────────┐  │
                    │  │ API/SSE │ │ Harness │ │ Blob   │  │
                    │  │  lane   │ │ loop    │ │ stream │  │
                    │  └─────────┘ └─────────┘ └────────┘  │
                    └───────┬───────────────┬──────────────┘
                            │               │
                   PostgreSQL (single DB)    local NVMe CAS
                   events/checkpoints/blobs  originals/large blobs
```

**为什么不是每用户一个进程/容器**：2GB / 200 租户 = 每租户 10MB，Go runtime 自身就要
10–20MB，进程模型从算术上就不可能。逻辑隔离（`tenant_id` + RLS + 配额）是唯一可行解。

**为什么不是每个租户一个 SQLite**：200 个 SQLite 各有 page cache（默认 2MB→400MB），
且无法跨用户做统一的事务/备份/配额；SQLite 的嵌入性在这个场景没有价值。Owner 2026-11-05 裁决。

## 3. 无状态执行模型（从 TS spike 继承的已验证语义）

TS spike（pi-durable）已用 7 个 E2E 场景证明以下语义可行（报告：
`.agents/reports/hive/2026-11-05-stateless-loop-e2e/`），Go 版按同一语义重做：

- **提交即可见**：未提交的部分不展示；事件日志是唯一真值，checkpoint 是投影。
- **每个 await 点可重入**：工具执行、模型流、生成任务都是持久任务；进程死了由 `resume()` 重放。
- **模型请求 = 已提交消息的纯函数**：重试不依赖进程内计数器。
- **幂等效果**：工具需自带幂等键（replay-safe ≠ exactly-once，框架不假装保证）。
- **请求幂等**：`requestId` 唯一约束，跨进程重投返回同一 submission。
- **断线续传**：SSE `Last-Event-ID` + 事件 offset；客户端缺口用 `before` 边界 + 分页补齐。

## 4. 存储：PostgreSQL 单库 + 分块大对象

已验证（PG 18，2GB 容器，本机 NVMe）：

| 方案 | 结果 | 结论 |
|---|---|---|
| 单行整图 `bytea` | 98.6 TPS（c16），整图 materialize | ❌ 1000 并发下内存与吞吐双杀 |
| **64KB 分块 `bytea`**（PK `(tenant_id, image_id, seq)`） | **1603 TPS (c16) / 1122 TPS (c32)** | ✅ 采用 |
| Large Objects（`lo_*`） | 每流占用一个连接+事务 | ❌ 1000 并发连接不可行 |
| 本地 NVMe 文件 | 比 PG 大 BLOB 快 3–10× | ✅ 大文件/原图走 CAS |

**分块行定义**（草稿，迁移文件为准）：

```sql
CREATE TABLE blobs (
  tenant_id   uuid NOT NULL,
  blob_id     uuid NOT NULL,
  sha256      bytea NOT NULL,
  size        bigint NOT NULL,
  mime        text NOT NULL,
  storage_kind smallint NOT NULL,   -- 1=pg_chunked 2=local_cas
  lane_class  smallint NOT NULL,    -- 1=small(≤256KB) 2=large
  ref_count   int NOT NULL DEFAULT 0,
  created_at  timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz,
  PRIMARY KEY (tenant_id, blob_id)
);
CREATE TABLE blob_chunks (
  tenant_id uuid NOT NULL,
  blob_id   uuid NOT NULL,
  seq       int  NOT NULL,
  bytes     bytea NOT NULL,
  PRIMARY KEY (tenant_id, blob_id, seq)
);
```

- 6 个 chunk 一个 `ReadAt` 窗口（384KB）→ base64 流式输出；**绝不**整图 `make([]byte, size)`。
- `ref_count` 由事件引用维护；`expires_at` 供未引用 blob 的 TTL 回收。
- 原图（>1MB 或非会话必需）写本地 CAS（sha256 两级目录），PG 只存元数据。

**PG 2GB 调参**（实测稳定）：`shared_buffers=384MB`、`max_connections=60`、
`work_mem=4MB`、`maintenance_work_mem=64MB`、`wal_buffers=16MB`、`max_wal_size=1GB`、
应用侧 `pgxpool` 24–32 连接。

## 5. 图片流式与反压

已验证（Go 1.27.3，本机）：

| 场景 | 吞吐 | 每连接内存 |
|---|---|---|
| SSE 纯文本 1000 连接 | 43.2 MB / 29.4 KB | 稳定 |
| 流式图片 1000 连接 | 798–871 MB/s | 48–62 KB |
| 整图读入内存 | 592–866 MB（OOM 风险） | ❌ |
| 反压生效后 | 1.17 GB 峰值buffer上限 | 稳定 |
| 重连风暴 | 23.1 MB | 稳定 |

设计：

1. **全局带宽预算**：约 1GbE 的 70–80%（≈700–800Mbps），token bucket 在 `io.Copy` 的
   `Write` 路径上扣减；预算耗尽时 `Read` 自然阻塞 → 存储读取被反压。
2. **分道**：控制/SSE 小包走快车道；图片/大文件走慢车道（独立 worker 池 + 独立预算）。
3. **尺寸感知调度**：请求进入调度器时按 `size` 分桶；小请求不排在 1MB 图片后面。
4. **有界 worker**：每类任务固定 `GOMAXPROCS×N` worker；队列满 → 429/503 + `Retry-After`，
   不无限堆 goroutine。
5. **慢客户端**：写超时（如 15s 无进展）+ 进度超时（如 30s 内 <1KB）→ cancel；
   已发送 offset 落 PG，支持 Range 续传。
6. **重试预算**：客户端 429 退避 + 抖动；服务端不自动重放大文件。
7. `GOMEMLIMIT=1.6GiB`、`GOGC=50–100`；RSS 曲线进验收报告。

## 6. 文件协议：先传图，后对话

- `POST /v1/files`（multipart 或 raw body）→ 返回 `file_id` + `url`；PG 元数据先落，chunks
  流式写入。
- 对话消息引用 `file_id`/`url`，**不重复上传**图片字节。
- provider 适配层：支持 OpenAI `image_url` 的 base64/data URL 与 URL 两种形态；
  不支持 URL 的 provider 由本仓在**出站**时流式转 base64（仍不整图进内存）。
- pi 生态的 `ImageContent` 是 base64/data URL 形态，没有独立文件协议——这是我们要补的层。

## 7. pigo 搬运清单

| 模块 | 搬什么 | 怎么改 |
|---|---|---|
| `provider` | OpenAI/Anthropic/OpenRouter/Ollama 协议、流式事件 | 去掉全局配置/环境变量耦合；注入 `http.Client`；预算接入反压 |
| `runtime` | 两层循环、工具轮、停止原因 | 状态外置到 PG；每轮可重入；取消用 context |
| `agenttool` | 并行/流式执行、结果裁剪 | 副作用幂等键；超时与重试预算 |
| `compaction` | 上下文压缩 | 压缩事件追加（不覆盖）；token 预算 |
| `agentcore` | text/thinking/toolCall/toolResult/image | 增加 `FileRef` 类型 |

不搬：CLI/TUI/REPL、JSONL session、trust、plugin/pkgmgr、selfupdate、内置 coding 工具。

## 8. 实测结果（2026-11-05）

| 指标 | 结果 |
|---|---|
| 1000 会话（SSE） | 0 异常断开 / 120s |
| 图片 | 1MiB × 2867 张完成；4.0GB base64 出站 |
| 吞吐 | 33.4 MB/s（客户端限速 100KB/s/流；网络未饱和） |
| 延迟 | p50 14.1s / p95 16.0s / p99 22.6s（≈ 1.4MB÷100KB/s，服务端附加≈0） |
| RSS | p50 300MB / p95 431MB / max **437MB**（GOMEMLIMIT 1.6GiB） |
| 429 | 588 次（满容量快速拒绝 + 客户端重试） |
| 无状态 E2E | 4/4 跨 SIGKILL（幂等、SSE 游标、blob 续传、2000 事件无缺口） |

调查结论：
- **专用文件格式/引擎不值得引入**（Haystack/SeaweedFS/LMDB/bbolt/Badger 的 RSS≈数据量或
  每读 4MB 缓冲，1GbE 下无收益）；本地 CAS + 页缓存 + PG 元数据是上限。
- **pi/pi-durable/pigo 全是 base64-only，没有 file 协议**；平台需自建 blob 层 + 每厂商 adapter。
- **反压**：网络是唯一显式上限；阻塞 Write + token bucket + per-write deadline 三者分工。

待办：
- agent loop 与事件日志的持久任务接线（工具执行/压缩的 resume）。
- provider 文件协议 adapter（Gemini → OpenAI Responses → Anthropic URL → …）。
