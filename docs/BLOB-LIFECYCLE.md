# Blob 生命周期：引用计数、TTL GC、缩略图

> 变更：`openspec/changes/keel-ga/specs/blob-lifecycle/spec.md`（§5）。
> 代码：`internal/store/blob*.go`、`internal/kernel/blobrefs.go`、`internal/blob/{gc,thumb}.go`。
> 本文是运维/评审口径：目录布局、删除顺序、幂等与失败重试。

## 1. 数据与目录

| 事实 | 存放位置 |
|---|---|
| blob 元数据（tenant, blob_id, sha256, size, mime, storage_kind, complete, ref_count, expires_at） | PG `blobs`（`(tenant_id, blob_id)` 主键，`(tenant_id, sha256)` 唯一） |
| 64KiB 分块（`storage_kind=1`） | PG `blob_chunks` |
| 大对象（`storage_kind=2`） | 本地 CAS 文件 |
| 引用事实 | PG `blob_refs`（`(tenant_id, blob_id, session_id, event_seq)` 主键） |
| 缩略图状态 | `blobs.thumb_status` / `blobs.thumb_blob_id` |

CAS 根目录由 `KEEL_CAS_DIR` 指定；未设置时 blob 仍可写 PG 分块，大对象退回 PG 分块。

```
$KEEL_CAS_DIR/
├── aa/bb/<64-hex>        # blob CAS：sha256 十六进制按前 2/次 2 位分目录
├── …                     # 只有两字符十六进制目录属于 blob CAS
└── layerfs/              # layerfs 自己的 CAS 子树（refs.log/.tmp/…）
```

**边界**：blob GC 只扫描 `CASDir/<2-hex>/<2-hex>/<64-hex>`。`layerfs/` 是另一个存储
（有自己的 refs/GC），任何 blob 路径都不会进入该子树；`docs/BLOB-LIFECYCLE.md` 的
orphan 扫描按“目录名必须是 2 位十六进制、文件名必须是 64 位十六进制”结构性排除它。

同一 sha256 的 CAS 文件是**跨租户共享**的（内容寻址）；数据库行按租户各自持有。
因此删除文件前必须做一次无租户谓词的 `CASBlobExists(sha256)` 复核。

## 2. 引用计数（由事件维护）

唯一写入口是契约内核的 append 事务（`kernel.PGStore` 的 `pgTxn.Append`）：

1. 事件 INSERT；
2. `referencedBlobIDs(kind, payload)` 解析 `user.message` / `assistant.message` /
   `tool.result` 中任意嵌套的 `"blobId"` 与 `"fileId"` 字符串；
3. 对每个 id 执行 `Store.AddRefTx`，**在同一个事务内**提交。

`AddRefTx` 的一条 SQL 完成三件事：

```sql
WITH ins AS (
  INSERT INTO blob_refs (tenant_id, blob_id, session_id, event_seq)
  SELECT $1, $2, $3, $4 FROM blobs b
  WHERE b.tenant_id = $1 AND b.blob_id = $2 AND b.complete
  ON CONFLICT DO NOTHING
  RETURNING blob_id
)
UPDATE blobs SET ref_count = ref_count + 1
WHERE tenant_id = $1 AND blob_id IN (SELECT blob_id FROM ins)
```

- 跨租户 / 未知 / `complete=false` 的 blobId：INSERT 选不中，零引用；事件本身仍按
  既有 payload 校验语义提交（客户端数据里多一个无效 id 不会毒化会话）。
- 同一 `(blob, session, eventSeq)`：`ON CONFLICT DO NOTHING`，重放/重试只 +1。
- 事件回滚 → 引用回滚；事件提交 → 引用同事务可见，不存在“事件已落账但引用丢失”的窗口。
- `blob_refs` 对 `blobs (tenant_id, blob_id)` 有 `ON DELETE CASCADE` 外键，只保证
  **“先插入引用、后 sweep”** 方向：引用的 INSERT 对 blob 行取 `KEY SHARE` 锁，sweeper
  的 `FOR UPDATE` 无法越过它删行。
- 反方向（sweeper 已锁行并删除，引用 INSERT 的 FK 检查随后失败，SQLSTATE 23503）是
  已容忍的竞态：`AddRefTx` 在 savepoint 中执行该引用写，23503 只回滚 savepoint、返回
  nil，事件照常提交且零引用（与“未知 blobId 忽略”语义一致）；不会出现“引用插入失败
  导致整条 user.message/tool.result 被拒”。
- 确定性排序（UUID 字符串序）保证并发事件对同一组 blob 的行锁顺序一致。

## 3. TTL 回收

### 3.1 候选

```sql
ref_count = 0
AND complete
AND expires_at IS NOT NULL AND expires_at < now()
AND NOT EXISTS (SELECT 1 FROM blobs p
                WHERE p.tenant_id = b.tenant_id
                  AND p.thumb_blob_id = b.blob_id
                  AND p.blob_id <> b.blob_id)
ORDER BY expires_at LIMIT n FOR UPDATE SKIP LOCKED
```

- 仍被引用（`ref_count>0`）、上传未完成（`complete=false`）、没有 `expires_at`
  的对象永不入选。
- 被父 blob 的 `thumb_blob_id` 指着的缩略图，即使自身 `expires_at` 已过也不入选。
  删除父行时缩略图可以同事务回收，但回收前还会复核：该缩略图 `ref_count=0`，且没有
  本批之外的父指向它；否则只删父（保护两父共享的缩略图，以及缩略图去重命中用户已有
  blob 的情况）。
- `SKIP LOCKED` 允许多个进程同时 sweep，互不等待。

### 3.2 删除顺序（严格）

1. 事务内：`blob_refs` → `blobs`（metadata）→ `blob_chunks`；
2. COMMIT；
3. 事务外：`CASBlobExists` 复核后 unlink CAS 文件（含缩略图文件），并尝试删除空目录。

即 **metadata 先删，chunks/CAS 文件后删**。崩溃点分析：

| 崩溃点 | 结果 | 恢复 |
|---|---|---|
| metadata 事务内任意失败 | 整批回滚 | 下一轮 sweep 重新选中 |
| metadata 已提交、chunks 删除前 | chunks 行成为孤儿（blobs 行已无） | metadata 已无 → 不影响可见性；由 orphan CAS/运维清理，见下 |
| chunks 已删、CAS unlink 前/失败 | 文件不可达 | orphan CAS 扫描（默认 1h grace）删除 |

chunks 与 metadata 在同一事务内，实际上不会出现“metadata 已提交但 chunks 留下”的
持久状态；表里的行用于说明顺序约束。**重复 sweep 幂等**：候选已不存在，返回 0。

### 3.3 重试机制：orphan CAS 扫描

`SweepOrphanCAS(grace)` 遍历两字符十六进制目录，只处理
`<64-hex>` 文件且 mtime 早于 `now-grace` 的项；文件对应 sha256 在 `blobs`
（`storage_kind=2`，跨租户）无任何行时删除。它同时承担：

- CAS unlink 失败/崩溃后的重试；
- 历史崩溃遗留文件的清理；
- 绝不触碰 `layerfs/`（目录名不是 2 位十六进制）与新鲜文件（in-flight 写）。

### 3.4 接线与配置

`keel serve` 与 `cmd/runtime` 启动两个后台循环，随进程 ctx 退出：

| 环境变量 | 默认 | 作用 |
|---|---|---|
| `KEEL_BLOB_GC_INTERVAL` | `1m` | TTL sweep 周期 |
| `KEEL_BLOB_ORPHAN_INTERVAL` | `30m` | orphan CAS 扫描周期（grace 固定 1h） |

## 4. 缩略图

### 4.1 状态机

```
上传完成（jpeg/png/gif） → thumb_status=pending
                               │ Thumbnailer（异步）
                               ├── 成功 → ready, thumb_blob_id=<新 blob>
                               └── 失败 → failed（终态，不重试风暴）
非图片 / 未完成            → none
```

- 原图行永不改写（mime/size/sha256 不变）；缩略图是**独立 blob 行**
  （`mime=image/png`，`complete=true`，`ref_count=0`），复用现有存储/读取/租户路径。
- **新建**的缩略图行写入时带 `expires_at=now+24h` 作为泄漏兜底：父行存在时被
  `thumb_blob_id` 谓词保护；若父行在 ready 写入前被 GC（极窄窗口），24h 后作为
  普通过期孤儿被回收。
- 若缩略图字节与同租户既有 blob 同 sha（`PutDetailed` 去重命中），生成器只记录
  `thumb_blob_id`，**不修改**该既有行的 `expires_at`/`ref_count`：它可能正是用户上传的
  原图，孤儿 TTL 只属于生成器真正新建的行。
- `failed` 只由 `pending` 迁入，周期扫描只取 `pending`，所以坏图不会重复生成。
- 生成失败只影响缩略图状态：上传响应、原图下载、对话事件提交都不经过生成器。

### 4.2 参数与限制

- 最长边 320px；小图不放大；双线性缩放（标准库实现）。
- 解码仅 `image/jpeg|png|gif`；`image.DecodeConfig` 先读尺寸，像素上限 25MP，
  编码字节上限 64MiB。
- 生成由上传完成通知 + 周期扫描（`KEEL_BLOB_THUMB_INTERVAL`，默认 30s）驱动；
  单进程串行，避免多个大图同时解码。

### 4.3 HTTP

`GET /v1/files/{id}/thumbnail`（沿用 `X-Tenant-ID` 租户谓词）：

| 状态 | 含义 |
|---|---|
| `200 image/png` | ready，正常流式返回 |
| `202` | pending（JSON `{"status":"pending"}`） |
| `404` | 该租户没有此 blob、此 blob 无缩略图（none），或 ready 但缩略图行已不存在 |
| `409` | failed（终态），或 ready 但元数据没有 `thumb_blob_id`，或缩略图对象未完成 |

上传响应与 `GET /v1/files/{id}/meta` 增加 `thumbStatus` / `thumbnailUrl` 字段。

## 5. 测试与证据

- 引用：`internal/kernel/blobrefs_test.go`（重放只 +1、两个事件 +2、跨租户/未完成/未知零引用、
  23503 下事件仍提交，用 trigger 确定性模拟）、`internal/store/blob_refs_test.go`（真实
  sweeper 锁行与 AddRef 交错：AddRef 返回 nil 且零引用）。
- GC：`internal/blob/lifecycle_test.go`（metadata/chunks/CAS 全删、重复 sweep 幂等、
  ref_count>0 与 complete=false 保护、租户隔离 + 跨租户内容共享、缩略图随父删除、
  共享缩略图/被引用用户 blob 保护、orphan 扫描只碰 blob CAS）。
- 缩略图：`internal/blob/thumb_test.go`（4000px → 320x240、原图字节不变、坏图 failed、
  非图片 none、dedupe 命中不改用户行 expiry、取消请求 ctx 仍排队）、
  `internal/httpapi/thumbnail_test.go`（200/202/404/409 与租户隔离）。
- base64 续传：`internal/blob/base64_offsets_test.go`（0/3 倍数/chunk 边界/末尾 +
  1000 个随机 4 倍数 offset + fuzz 属性）。
- 门禁输出：`openspec/changes/keel-ga/evidence/blob-lifecycle/`。
