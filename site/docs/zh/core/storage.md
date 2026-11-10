# 存储与运行

存储决定内存预算的胜负，也是「账本是唯一真值」落地为具体 SQL 的地方。本页覆盖 PostgreSQL schema、
blob 层（库内分块 + 内容寻址溢出）、准入分池、资源预算，以及支撑这些结论的实测数字。

## 1. PostgreSQL：单库 + 逻辑隔离

Keel 使用**单个 PostgreSQL 18 库**，每行带 `tenant_id` 与复合键，而不是每租户一个库或进程
（`README.md:19`）。行级安全（RLS）可选；主要隔离手段是复合键加内核的租户校验。

为什么不是每租户一个进程或 SQLite 文件：2GB ÷ 200 租户 = 每租户 10MB，而 Go runtime 自身就要
10–20MB——算术上直接否决。200 个 SQLite 各带 page cache（默认 2MB → 400MB），且无法共享统一的事务/
备份/配额视图（`docs/ARCHITECTURE.md:32`）。

schema 分两处：

- `internal/store/schema.sql` —— 基础表；
- `internal/kernel/schema/0001…0005*.sql` —— 内核迁移：fencing、工具调用索引、外部/交接、待读输入、
  loadout revision。迁移在启动时于事务内执行（`SET LOCAL statement_timeout=0`），锁等待由
  `lock_timeout=30s` 兜底并在 `55P03` 上自动重试（`docs/RUNBOOK.md:188`）。

| 表 | 用途 | 关键细节 |
|---|---|---|
| `tenants` | 租户行 + `config jsonb` + `config_revision` | revision 是变更身份；不叠加 ETag/mtime 语义 |
| `sessions` | 每会话一行；`next_seq` 分配事件序号 | 同时承载外部标记：`state_owner`、`external_backend`、`external_ref`、`external_status`、`metering` |
| `events` | **账本**：`(tenant_id, session_id, seq)` 主键，`kind`、`payload jsonb`、`dedupe_key` | 部分唯一索引 `events_dedupe`（`(tenant_id, session_id, dedupe_key)`，非空时生效） |
| `checkpoints` | 压缩摘要，落到 `upto_seq` | 投影，永不是真值源 |
| `submissions` | 请求幂等 `(tenant_id, session_id, request_id)` | 重试的请求只有一条 submission |
| `tasks` | durable 任务行（submission/生成/嵌套工具/结构化输出） | 当前已有的 PG 投影 |
| `runs` | 每会话一行：`status`、`lease_until`、`attempts`、`cursor_seq`、`last_error`、`pending_input`、`read_watermark`、`loadout_*` | 用 `FOR UPDATE SKIP LOCKED` 领取；进程崩溃留下租约，任何 worker 都可回收 |
| `blobs` / `blob_chunks` / `blob_refs` | blob 元数据 + 64KiB 分块行 + 以事件为键的引用计数 | `UNIQUE (tenant_id, sha256)` 去重上传；`STORAGE EXTERNAL` 让已压缩字节免于 TOAST |
| `usage_ledger` | 每次 provider 请求一行，以 runner 生成的 `request_id` 为键 | 刻意**没有价格列**；usage 恢复后把 `unaccounted` 行原地升级 |
| `mcp_oauth_pending` / `mcp_oauth_credentials` | MCP OAuth 状态机与加密凭据 | AES-256-GCM，AAD = tenant+server |
| `model_settings` / `model_settings_history` / `tenant_secrets` | 租户模型文档 + 密钥存储 | 密钥加密存储；配置里只出现引用 |

两处数据库侧行为对运行时很重要：

- **追加即通知**。`AFTER INSERT` 触发器发 `pg_notify('keel_events', tenant:session:seq)`
  （`internal/store/schema.sql:207`）；订阅者按自己的游标重读日志，因此丢一次通知只损失一个心跳周期。
- **就绪检查深入到 schema**。`GET /v1/ready` 不只 Ping PG，还检查运行 schema（内核迁移
  0001/0003/0004/0005 的列、外部会话标记、handoff 表）。缺任一列的旧库返回 503，而不是假装就绪
  （`docs/RUNBOOK.md:275`）。

## 2. Blob：库内 64KiB 分块 + CAS 溢出

存储选型是**测出来的**，不是猜的（`docs/ARCHITECTURE.md:52`）：

| 方案 | 结果 | 结论 |
|---|---|---|
| 单行整图 `bytea` | 98.6 TPS（c16），整图 materialize | 否决——1000 并发下内存与吞吐双杀 |
| **64KiB 分块 `bytea`**（PK `(tenant_id, image_id, seq)`） | **1603 TPS (c16) / 1122 TPS (c32)** | 采用 |
| Large Objects（`lo_*`） | 每流占用一个连接 + 事务 | 否决——1000 并发连接不可行 |
| 本地 NVMe 文件 | 比 PG 大 BLOB 快 3–10× | 采用：大图/原图走 CAS |

上传器的规则（`internal/blob/upload.go:39`、`:58`）：

- **≤1MiB** —— spool 到临时文件（内存恒定），SHA-256 去重，然后按 64KiB 分块写入 PostgreSQL；
- **>1MiB** —— 移入本地**内容寻址存储**（`CASPath`，sha256 两级目录，`internal/blob/upload.go:24`），
  PostgreSQL 只存元数据（`storage_kind` = `pg_chunked` | `local_cas`）；
- 上传按 `sha256` 在租户内去重；`ref_count` 由 `blob_refs` 维护，其键包含 owning event，因此不可能
  重复加一；未被引用的 blob 经 `expires_at` 走 TTL 回收。

**读取两端都是流式的**。base64 读取器（`internal/blob/stream.go:116`）按 chunk 序号走 blob，整块
base64 编码（4 个 base64 字符 ≡ 3 个源字节，这正是任意 wire offset 可续传的原因），再用 `io.Copy`
写出。整图永不进内存；读批大小由 `BufferPool` 承载（64KiB × batch）。

**Range 续传由客户端驱动**：客户端上报已收到的 wire offset（`Range: bytes=<off>-`，必须是 4 的倍数），
服务端映射回源偏移（`srcOff = off/4*3`），返回 `206 + Content-Range`，`totalWire = 4·⌈n/3⌉` 取自
元数据。base64 输出**不加换行**，否则 4:3 关系被破坏（`docs/BACKPRESSURE.md:102`）。

**不引入专用存储引擎**。调研结论很明确：Haystack/SeaweedFS/LMDB/bbolt/Badger 的 RSS ≈ 数据量，或
每读一次占 4MB 缓冲，在 1GbE 下没有收益；本地 CAS + 页缓存 + PG 元数据就是上限
（`docs/ARCHITECTURE.md:152`）。完整权衡研究见 `docs/BLOB-STORAGE-RESEARCH.md`。

## 3. 准入：分池，不排队

请求按尺寸分类——Small <256KiB、Medium <2MiB、Large ≥2MiB——软预约 640/300/60，硬上限 900/400/80。
准入要求本类未超硬上限，**且**为其他类预留未满软额后仍有余量。队列上限 256；溢出立即返回
`429 + Retry-After`，客户端按抖动退避重试（`docs/BACKPRESSURE.md:57`）。

让 SSE 与 JSON 在一个进程里自洽共存的 HTTP 服务器配置（`docs/BACKPRESSURE.md:76`）：

| 字段 | 值 | 理由 |
|---|---|---|
| `ReadHeaderTimeout` | 5s | slowloris |
| `ReadTimeout` | 0 | SSE 会被腰斩；JSON 路由自带读期限（30s） |
| `WriteTimeout` | 0 | 它是整段响应的绝对期限，SSE 必死；写期限改为逐次写 |
| `IdleTimeout` | 75s | keep-alive |
| `MaxHeaderBytes` | 16KiB | |
| 内核 sndbuf | `SetWriteBuffer(64KiB)` | 1000 连接 → 约 128MB 内核内存，而不是数 MB 的自适应默认值 |

慢客户端判定分两级：单次写最多阻塞 4s；15 秒窗口内增量低于 180KB（12KB/s）连续两个窗口即判慢；
总时长只作兜底（`docs/BACKPRESSURE.md:90`）。超时后直接 abort 连接
（`panic(http.ErrAbortHandler)`），而不是往半写的 chunked 帧里写错误页。

## 4. 资源预算

每流内存，按 1000 流计（`docs/BACKPRESSURE.md:108`）：

| 项 | 字节 |
|---|---|
| 读批缓冲（batch=2×64KiB） | 131,072 |
| pgx 解码行（瞬时） | 65,536 |
| base64 输出缓冲 | 87,384 |
| net/http bufw | 2,048 |
| goroutine 栈 | 8,192 |
| handler/header/timer | ~4,096 |
| TLS 记录缓冲 | ~17,408 |
| **每明文流** | **~227KiB** |
| **每 TLS 流** | **~245KiB** |

1000 条活跃流 ≈ **227–245MB 用户态 + 128–160MB 内核** ≈ **~390MB**——且 `GOMEMLIMIT` **不覆盖**
内核 socket 缓冲，两者必须分开记账。塑造预算的运行时开关（`cmd/runtime/main.go:43`）：

```bash
GOMEMLIMIT=1610612736 ./keel-runtime \
  -addr 127.0.0.1:13740 -cas /tmp/keel-cas \
  -streams 2200 -chunk-batch 1 -admit-rate 1200 -runs 64
```

- `-streams` —— 总并发流上限（1000 会话 SSE 加图片流需要 1600–2200）。
- `-chunk-batch` —— 每查询批取块数：`1` ≈150KB/流；`2` 是 1Gbps 模式（查询数减半）。
- `-bandwidth` —— 全局出站字节/秒（0 = 不限；生产按链路 85–90% 设）。
- `-runs` —— 并发 agent run 数；`-no-worker` 只服务 API。

**2GB 档的 PostgreSQL 调参**（实测稳定）：`shared_buffers=384MB`、`max_connections=60`、
`work_mem=4MB`、`maintenance_work_mem=64MB`、`wal_buffers=16MB`、`max_wal_size=1GB`，应用侧
`pgxpool` 24–32 连接（`docs/ARCHITECTURE.md:90`）。连接池设置是设计的一部分：`MaxConns=24`
（朴素的 `max(4,GOMAXPROCS)` 太小，而 c32 实测反而掉 30%）、`MinConns=8`、
`MaxConnLifetime=30m`、`MaxConnIdleTime=5m`、`HealthCheckPeriod=30s`，并在连接上设
`statement_timeout=5s` 与 `idle_in_transaction_session_timeout=3s`（`docs/BACKPRESSURE.md:154`）。
取连接的 ctx 超时 300ms → `503 + Retry-After`，避免网络反压漏成慢查询雪崩。

## 5. 实测结果

2GB / 1000 会话基线，取材自 `README.md:26` 与 `docs/RUNBOOK.md:137`（2026-11-06 与 2026-11-08
终轮；两轮负载构成不同，故都列出）：

| 指标 | 2026-11-06 基线 | 2026-11-08 终轮 |
|---|---|---|
| 并发会话（SSE） | 1000，0 硬错误 | 1000，0 硬错误 |
| 图片 | 800×1MiB 完成（1.12GB base64 出站；200 张按分池返回 429 后被重试） | 998/1000 完成（1853 次 429 重试） |
| 吞吐 | 18.6 MB/s（客户端限速主导） | 23.2 MB/s |
| 延迟 | p50 28.9s / p95 31.4s（= 1.4MB ÷ 50KB/s，服务端附加 ≈0） | — |
| 2GB cgroup 峰值 | 544MB | **640MB（29.8%）**，上限门 1.5GB |
| 双进程 handoff 峰值 | — | 656MB（32.0%） |

`429` 是尺寸分池的预期反压而非流错误；压测工具按 `Retry-After` 重试。`docs/MATURITY.md:23` 定义
GA 规模条件（2GB cgroup、1000 SSE、64 并发 run、周期 1MiB 图片、RSS 峰值 < 1.8GB、0 OOM）。

无状态 E2E 为 7/7：`requestId` 幂等、SSE 游标、blob wire-offset 续传、agent 回复、模型调用中
`SIGKILL`、工具执行中 `SIGKILL`、100 会话 × 2000 事件无缺口（`docs/RUNBOOK.md:112`）。

## 6. 运行运行时

| 面 | 位置 |
|---|---|
| 存活 | `GET /v1/health`（恒 200；用作 k8s liveness） |
| 就绪 | `GET /v1/ready`（PG + schema；排水中 503；用作 k8s readiness） |
| 指标 | `GET /metrics`（Prometheus text 0.0.4）与 `GET /v1/metrics`（JSON） |
| 排水/接管 | `POST /v1/runtime/drain`、`GET /v1/runtime/status` |
| 优雅停止 | SIGTERM：停 claim、新 API 请求 503、先补发 SSE、发 `event: drain`、按速率关闭；SIGINT 立即取消 |

指标族刻意保持固定基数——**禁止** tenant/session/run 标签（`docs/RUNBOOK.md:233`）：
`keel_runs_started_total`、`keel_runs_finished_total{status}`、`keel_run_attempts_total{outcome}`、
`keel_run_attempt_duration_seconds`、`keel_runs_pending`、`keel_streams_active`、`keel_draining`、
`keel_leases_expired_total`、`keel_compactions_total`，外加 Go runtime 与 `pgxpool` gauge。僵尸
attempt（租约被接管或终态已提交）会被 finish 的 `status='running'` + `attempts` 守卫拒绝
（`ErrStaleRun`），计入 `abandoned`，且**不写**终态。

`/metrics`、`/v1/health`、`/v1/ready` **没有鉴权**：绑回环或在代理层限源。完整运行手册（测试套件、
压测、混沌、2GB cgroup 复跑、镜像与 compose 流程）见 `docs/RUNBOOK.md`。
