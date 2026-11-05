# 专用文件格式 / 存储引擎调研结论

> 来源：2026-11-05 调研（Haystack/f4、SeaweedFS、Garage/MinIO/RustFS、LMDB/bbolt/Badger/
> RocksDB、restic/casync/Bazel CAS、本地 FS + page cache；含本机 1000×1MiB 基准）。
> **结论：本地 CAS + 页缓存 + PG 元数据已是最优；不引入专用格式/引擎。**

## 1. 各方案事实

### Haystack / f4
- Haystack（OSDI'10）：260 亿图/20PB；内存索引均值 10B/张（4 尺寸合计 40B/原图），
  XFS inode 536B；每次读 1 次盘 I/O；全集群峰值 >100 万图/s；append-only needle 卷 +
  内存索引 + 索引文件快速重启。
- f4（OSDI'14）：只收 warm BLOB，65PB；复制因子 3.6→2.8/2.1（RS 纠删 + 异地 XOR），
  **明确牺牲读吞吐**，不适合 1000 热流。
- 无官方 Go 实现；SeaweedFS 自述源自 Haystack，是 Go 最接近的继承者。

### SeaweedFS（Go，Apache-2.0，v4.52）
- volume/needle append-only；16B/blob 内存索引；O(1) 一次盘读。
- **每次读分配 `min(4MB, needle大小)` 缓冲**（`weed/storage/volume_read.go:213`）；
  1000×1MB ≈ 1GB 缓冲，2GB 机器顶爆；`concurrentDownloadLimitMB` 默认不限。
- 可调小缓冲，但那就退回自己写文件流。

### Garage / MinIO / RustFS
- Garage：Rust/AGPL，元数据 LMDB，定位中小自托管；单机 quick-start 明确「不应生产」。
- MinIO：社区仓 `archived=true`（2026-04-24），AIStor 转商业。
- RustFS：v1.0.4（2026-10-25），太新，2GB 单机生产验证未取证。
- 共性：多一跳 S3/HTTP、额外进程/元数据；1GbE 下吞吐无收益。

### 嵌入式 KV
| 引擎 | 内存模型（100MB 预算） | 1000×1MB 读（暖缓存） | 问题 |
|---|---|---|---|
| bbolt | 堆 2MB；RSS≈1.02GB/1GB 数据 | 1.69GB/s | mmap 文件页计入 RSS；长读事务阻止旧页回收 |
| Badger | 堆 24–47MB；RSS≈1.02GB | 1.35GB/s | vlog 2 跳 I/O + GC 重写 |
| LMDB | 堆 4–6MB；RSS≈1.12GB | 0.50GB/s | cgo；长读事务使 DB 膨胀 |
| RocksDB | 可配 block cache | 大 value 写放大/解压 | cgo；Get 返回副本 |

**都不满足「既快又省内存」**；1000 流会把长事务问题放大。

### 打包格式 / CAS
- restic：pack 默认 16MiB，blob 按内容哈希 + offset/len 索引；内部包不可直接当库。
- casync：内容定义分块，面向镜像分发，不是 1000 流服务。
- Bazel CAS / Perkeep：按 digest 寻址，无 pack 或索引过重。
- pack+index 是唯一值得借鉴的形态：1M blob 的最简索引约 10–28B/条 = 10–28MB，
  可常驻 100MB。**但只有「对象数 >100 万」时**才比 inode（256–536B/文件）有显著优势；
  1MB 图占主导时存储碎片 <0.4%，收益可忽略，却引入 GC/崩溃恢复/并发写。

### 本地 FS + page cache（本机实测）
Mac 8C/16GB，GOMAXPROCS=2，GOMEMLIMIT=100MiB，暖 APFS，1000×1MiB base64→io.Discard
（非 HTTP/TLS/2GB cgroup）：
- 16KB 缓冲：1.23–1.40GB/s，maxrss 19MB。
- 32KB 首轮：605MB/s，maxrss 55MB。
- 单核：951MB/s。
- 整文件 `ReadFile+EncodeToString`：1000 并发峰值 **2.32GB**（无限制）/
  349MB + 343 次 GC 且 3× 慢——**绝对不要**。
- `sendfile/splice` 对 base64 不可用（必须变换），只能 16–32KB 用户缓冲。

> 纠偏：既有「62–75MB / 798–871MB/s」是 loopback/10GbE 数字；真实 1GbE 理论 125MB/s、
> TCP 约 118MB/s，1000×1MiB base64=1.398GB ≈ 11.8s（平均 ~118KB/s/流）。
> **网络才是瓶颈。**

## 2. 最终架构

- **存储**：本地 CAS 文件（SHA-256，两级 `ab/cd/<hash>`），写临时文件 + fsync + rename；
  PG 只存 `hash/size/mime/refcount`，热路径不查 BLOB。
- **读**：`os.Open` + 流式 base64（16–32KB 缓冲，有界 freelist），
  `Content-Length = 4*ceil(size/3)`；绝不 `ReadAll`/`EncodeToString`。
- **PG 分块 BLOB**：保留给「必须与事务同生共死」的小图（≤1MB）；不作为大文件热路径
  （1MB≈524 个 2KB TOAST 块，1000 流需要千级后端/连接，池化后排队）。
- **缓存预算（100MB 应用侧，不含 PG/OS）**：16MB 流缓冲池（1000×16KB）+ 8MB HTTP/scratch +
  16MB 元数据/ETag + 32MB 热小对象 LRU（只收 <64KB；1MB 原图不进 LRU，页缓存已是缓存）+
  28MB 运行时/突发。
- **运维**：`LimitNOFILE=65535`（1000 socket+1000 文件，默认 1024 必爆）；
  `GOMAXPROCS=2`；`GOMEMLIMIT=128–160MiB`（应用侧）；正常 buffered read，不用 O_DIRECT；
  删除用延迟 unlink。
- PG 若同机：`shared_buffers 128–256MB` + 16–32 后端另计，**必须从 2GB 总量扣除**。

## 3. 三条不要引入

1. 不要自研全量 Haystack/needle/pack——1MB 图占主导时只省 inode 与 <0.4% 碎片；
   百万对象、或 inode 缓存实测顶到预算时再做「旁路 pack」，不替换本地 CAS。
2. 不要用嵌入式 KV 存 1MB 图片——RSS≈数据量，长读事务与慢 HTTP 流冲突。
3. 不要单机 S3（SeaweedFS/Garage/MinIO/RustFS）——多一跳、额外进程；
   SeaweedFS 每读 1MB×1000=1GB，MinIO 已归档，RustFS 太新，Garage 单机不推荐。
