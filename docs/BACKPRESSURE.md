# 网络反压与请求分池（设计）

> 来源：2026-11-05 research 调查；数字来自本机实测（Go 1.27.3 / PG 18 / 1000 并发）
> 与推算，推断项已标注。结论直接约束 `internal/stream` 的实现。

## 0. 先立公式

- `b64(n) = 4·⌈n/3⌉`；64KiB 块 → **87,384 B**（+33.3%）
- 块 QPS = `链路B/s ÷ 87384`：1Gbps→1430，500Mbps→715，100Mbps→143
- 单流内存 = 读批 + b64 缓冲 + 2KiB + 栈 + TLS

**核心判断**：100–500Mbps 档可以做到「网络是唯一显式上限」；1Gbps 档 PG 会同时成为瓶颈
（实测 1603 TPS × 64KiB ≈ 820Mbps 已是单块查询天花板，c32 反而降到 575Mbps）。目标是
「网络为上限、PG 留 ≥20% 余量」，手段是**每查询批取 2–4 块**，而不是换游标。

## 1. 全链路反压：三种机制叠加，各管一层

| 机制 | 反压来源 | 管住 | 管不住 | 成本 |
|---|---|---|---|---|
| (a) 阻塞 `Write`（`io.Copy`） | 内核 sndbuf + 对端接收窗 | 单流驻留内存（天然有界） | 总带宽、公平性、死连接 | 0 |
| (b) 全局/租户 token bucket | 用户态记账 | 总带宽上限、公平、可观测排队 | 单流慢客户端 | 每块一次 `WaitN` |
| (c) `SetWriteDeadline` | 超时 | 死/僵客户端、goroutine 泄漏 | 带宽总量 | 每次 Write 一次 syscall |

**推荐组合：数据通路 (a) 阻塞，策略层 (b) 塑形，护栏 (c) 超时。**

「网络成为唯一上限」的三个必要条件：

1. 单 goroutine 拉模型，全链路无 channel/queue 缓冲；
2. **限制内核缓冲**：Linux `tcp_wmem` 可 autotune 到 4MB/连接，1000 连接 = 4GB 内核内存，
   直接打爆 2GB 机器 → `SetWriteBuffer(64<<10)`（内核翻倍 ≈128KB/连接，总 128MB）；
3. 整形器设在链路 85–90%，让队列形成在**用户态可观测**的位置。

```go
func (p *pw) Write(b []byte) (int, error) {
    if p.lim != nil {
        if err := p.lim.WaitN(context.Background(), len(b)); err != nil { return 0, err }
    }
    if err := p.rc.SetWriteDeadline(time.Now().Add(p.maxBlock)); err != nil { return 0, err }
    n, err := p.w.Write(b)      // 内核满则阻塞，反压原路上溯到 PG 读循环
    p.sent += int64(n)
    if err != nil { return n, err }
    // 每秒检查窗口速率，连续 2 个窗口低于 minRate → errSlowClient
    return n, nil
}
```

要点：
- bucket 的 **burst ≥ 最大单次 Write**（87,384），否则 `WaitN` 返回 “exceeds burst”；
  设 burst = 2×块 = 176KiB，rate = 链路×0.88。
- 桶按 **base64 之后的线上字节**计量。
- `SetWriteDeadline` 是绝对时刻，必须每次 Write 前重设，流结束置零。
- deadline 触发后分块帧已写一半，**不可恢复**：记录 offset 后 `panic(http.ErrAbortHandler)`
  （net/http 静默关连接），不要向同一响应写错误页。
- `ResponseController` 依赖 `Unwrap() http.ResponseWriter`；中间件包装器必须实现。
- 顺序固定：**bucket → deadline → Write**。

## 2. 按尺寸分池：三类 + 软预约 + 可借位

**不要单一优先级队列 + SJF**：小请求到达率趋近饱和时大请求饥饿，老化只是把饥饿变成抖动。

```go
type Class uint8
const (Small Class = iota; Medium; Large) // <256KiB / <2MiB / ≥2MiB
var soft = [3]int{640, 300, 60}
var hard = [3]int{900, 400, 80}

// admit(c) ⇔ cls[c] < hard[c]
//          ∧ inflight + 1 + Σ_{d≠c} max(0, soft[d]-cls[d]) ≤ cap
```

- 不饥饿：其他类不能吃掉 L 未用满的 60 个软名额。
- 不浪费：本类未满软额时 `reserved` 为 0，可吃满 `hard` 借位。
- 队头最多扫描 64 个 waiter；租户轮转防单租户占满。
- 队列上限 256，超过 **429 + Retry-After**；`qcap ≈ λ_peak × target_wait`。

`http.Server` 参数（SSE 与 JSON 混跑的唯一自洽解）：

| 字段 | 值 | 理由 |
|---|---|---|
| `ReadHeaderTimeout` | 5s | slowloris |
| `ReadTimeout` | 0 | SSE 会被腰斩；按路由 `SetReadDeadline`（JSON 30s） |
| `WriteTimeout` | 0 | 它是整段响应的绝对期限，SSE 必死；写期限走 per-Write |
| `IdleTimeout` | 75s | keep-alive |
| `MaxHeaderBytes` | 16<<10 | |
| 内核 sndbuf | `SetWriteBuffer(64<<10)` | 1000 连接 → 128MB 内核 |

注意：`net/http` 没有 `MaxConn`；用 `netutil.LimitListener` 或 `ConnState` 计数。
`http.MaxBytesReader` 只管入站请求体。

## 3. 慢客户端判定

阻塞时长（Linux）：
- 对端 RST/进程退出 → 立即。
- 对端宕机 → keepalive 约 15–150s 才断。
- 对端存活但接收窗 0 → **无限期**（keepalive 探测会被 ACK）。只有写期限能杀。

两级标准：
1. 单次 Write 超时 `maxBlock = 4s`（隐含下限 22KB/s）。
2. 15s 窗口增量 <180KB（12KB/s）连续 2 个窗口 → 判慢。
3. 总时长只做兜底（10min），不作慢判据。

续传 offset 由**客户端**报（服务端无法知道 sndbuf 里的数据是否送达）：
- 客户端维护 `wireOffset`（收到的 base64 字节数），重连发 `Range: bytes=<off>-`。
- `off % 4 == 0`，`srcOff = off/4*3`（4 个 base64 字符 ≡ 3 个源字节）。
- 服务端回 `206 + Content-Range`，`totalWire = 4*⌈n/3⌉` 从元数据取。
- base64 输出**不加换行**，否则 4:3 关系破坏。

## 4. 每流内存预算（1000 流）

**每流 1 个 goroutine**；pgx 查询同 goroutine 同步完成，禁止为预取开第二个 goroutine。

| 项 | 字节 |
|---|---|
| 读批缓冲（batch=2×64KiB） | 131,072 |
| pgx 解码行（瞬时） | 65,536 |
| base64 输出缓冲 | 87,384 |
| net/http bufw | 2,048 |
| goroutine 栈 | 8,192 |
| handler/header/timer | ~4,096 |
| TLS 记录缓冲 | ~17,408 |
| **合计/明文流** | **~227KiB** |
| **合计/TLS 流** | **~245KiB** |

1000 活跃流 ≈ **227–245MB 用户态 + 128–160MB 内核** ≈ **~390MB**。

- `GOMEMLIMIT` **不覆盖内核 socket 缓冲**，必须单独记账。
- base64 复用：`EncodedLen` + 预分配 dst + `Encoding.Encode(dst, src)` 整块编码
  （~1.5GB/s/核，1430 块/s 占单核 ~6%）；不要 `NewEncoder` 逐 4 字节写。
- `sync.Pool` 有 GC 清空导致「内存压力最大时命中率崩塌」的风险 → 用**有界 freelist**
  （channel + 容量上限 = 2×maxActive）。
- `Flush`：图片每 87KB 刷一次；SSE 按「≥4KiB 或 ≥50ms」合并；用复用 `time.Timer`，
  不用 `time.After`。
- 图片用 `Content-Length + Accept-Ranges`；SSE 只能 chunked（`X-Accel-Buffering: no`）。
- HTTP/2 有 HOL 阻塞与流控窗掩盖反压；1000 个独立客户端优先 h1。

## 5. PG 侧：短查询 + keyset 分页，绝不长事务流式读

pgx 的 `rows` 在 `Close()` 前独占连接；1000 流各持一个 rows = 1000 连接，PG 每后端
2–10MB 工作内存 → 2–10GB，且快照钉住 `xmin`。`DECLARE CURSOR` 更糟（结果物化在 PG 侧）。

1Gbps 需 1430 块/s，实测单块查询 c16=1603 TPS（余量 12%）、c32=1122（不够）。
**batch 不是优化项而是 1Gbps 档的准入条件**：batch=2 → 715 QPS，batch=4 → 358 QPS。

```go
const qBatch = `SELECT chunk_idx, bytes FROM blob_chunks
                WHERE tenant_id=$1 AND blob_id=$2 AND seq >= $3
                ORDER BY seq LIMIT $4`
// handler: fetch（拷出 bytes，rows.Close 归还连接）→ encode → write → 下一批
```

- **keyset（`seq >= $3` + `ORDER BY` + `LIMIT`）优于 OFFSET**：PK 范围扫描、O(log n)、
  并发稳定；OFFSET 深翻页要扫过前 N 行。
- **不走 cursor**：无事务、无连接占用、无快照膨胀；blob 按 `version` 不可变。
- pgxpool：`MaxConns=24`（默认 `max(4,GOMAXPROCS)` 太小；c32 反而掉 30%）、`MinConns=8`、
  `MaxConnLifetime=30m`、`MaxConnIdleTime=5m`、`HealthCheckPeriod=30s`；
  `AfterConnect` 设 `statement_timeout=5s`、`idle_in_transaction_session_timeout=3s`。
- `Acquire` 带 300ms ctx 超时 → 503 + `Retry-After`，不让网络反压漏成慢查询雪崩。
- 图片块不可变 → 只读副本是 1Gbps 档最省事的扩容杆（推断，需评估复制延迟）。
- `ALTER TABLE blob_chunks ALTER COLUMN bytes SET STORAGE EXTERNAL`（图片已压缩，禁 pglz）。

## 6. 重连风暴

1000 同时重连仅 23.1MB，内存不是问题；真正成本是 TLS 握手与准入后的鉴权/查询。

服务端：
1. 准入令牌桶 200/s、burst 400；429/503 + `Retry-After`（整数秒）+ `retry_after_ms`。
2. 重连快路径：会话状态内存 LRU（或 Redis），不查库；续传只校验 Range。
3. 主动排水：先写 `event: drain` + `retry_after_ms`，再按 100 条/s 分波关闭。

客户端：
- 首次连接随机铺开 0–1.5s。
- 失败 `full jitter`：`rand(0, min(20s, 400ms·2^n))`，同时尊重 `Retry-After`（取 max）。
- 连接稳定 >60s 才把 attempt 归零。

## 7. 可观测性

| 层 | 指标 |
|---|---|
| 流 | `bytes_sent_total`、`chunks_sent`、`write_block_seconds`、`slow_kills_total`、`resume_total` |
| 调度 | `active_streams{class}`、`queue_depth{class}`、`queue_wait_seconds`、`admission_rejects` |
| PG | `chunk_fetch_seconds`、`chunks_fetched`、`pgxpool.Stat()` |
| 运行时 | goroutines、heap、GOMEMLIMIT 距离、GC pause、freelist 占用 |

- **绝不**按 stream ID 打标签（6000 series 会炸）；流维度只保留全局聚合。
- 慢事件用固定容量环形缓冲（256 条 × 128B = 32KB），`/debug/streams?top=20` 按需快照。
- 写路径计时按 1/16 抽样；日志只在状态跃迁打。

## 8. 实施顺序

1. sndbuf + 超时参数 + 准入（零风险，立即止血）
2. 两阶段批取（决定 1Gbps 是否可能）
3. `pw`（bucket + deadline）+ 整形
4. 三类分池
5. 指标
