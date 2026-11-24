---
title: 文件与 Blob
---

# 文件与 Blob

Keel 有两层文件，回答的是两个不同的问题：

- **会话文件（session files）**是 Agent 的工作区：一棵按会话隔离的路径树，字节内容寻址。文件工具通过
  `fs.write` 写入；bash 会话的改动在结算时由 `fs.capture` 捕获；读取经 `file_manifest` 挂载。
- **上传 Blob（uploaded blobs）**是被事件 payload 引用的附件与媒体：用户上传、发给模型的图片、生成的
  音频。它们存在 `blobs` / `blob_chunks` / `blob_refs`，用 `GET /v1/files/{id}` 取回。

两层都是内容寻址、都可去重。账本是会话**引用了什么**的权威：manifest 行是已提交事件的投影，blob
引用由点名它的事件计数，没有被任何已提交事件引用的字节就是垃圾。

## 1. 两层对照

| | 会话文件 | 上传 Blob |
|---|---|---|
| 身份 | `(tenant_id, session_id, path)` → 内容哈希 | `(tenant_id, blob_id)`，唯一键 `(tenant_id, sha256)` |
| 路径投影 | `file_manifest`（`internal/store/schema.sql:855`） | 无——payload 里带 `blobId` / `fileId` |
| 快照 | `session_snapshot` 记录某个 `seq` 的 tree 哈希（`internal/store/schema.sql:871`） | 每个 blob 的 `expires_at`，引用行按事件 seq 记录 |
| 字节 | layerfs CAS，两种对象：文件字节与规范化 tree 列表（`internal/layerfs/doc.go:5`） | PG 64KiB 分块或本地 CAS 大对象 |
| 写入方 | 文件插件（`keel.file.read/write/edit/capture`） | `POST /v1/files`（`internal/httpapi/server.go:235`） |
| 落账 | `fs.write`、`fs.capture` 事件 | 事件 append 时自动 retain 被引用的 blob（`docs/BLOB-LIFECYCLE.md:33`） |
| 回收 | `Collect` 清理超过保留窗口的未引用对象；当前 shell 不调度它（`internal/layerfs/store.go:27`） | TTL 策略 + orphan CAS 扫描（`docs/BLOB-LIFECYCLE.md:69`） |

这个划分是刻意的：会话工作区必须扛得住 fork 与重放（它是一棵树），而附件必须扛得住被任意会话的
任意事件引用（它是一个 blob）。

## 2. 会话文件：manifest、tree 对象、snapshot

`file_manifest` 是挂载点，不是字节本身。每行一个 `(tenant, session, path)`，带哈希、mode、size、
建立该行的 `updated_seq`，以及可选的 `last_task_id`（`internal/store/schema.sql:855`）。字节在
layerfs CAS 里作为**文件对象**存放；目录列表是**tree 对象**——规范化渲染后按自身哈希寻址
（`internal/layerfs/tree.go:147`）。

一次 capture 产生一个 tree 对象，并把 `seq` 与该哈希写进 `session_snapshot`
（`internal/fileplugin/manifest.go:311`）。正是这一行让 fork 与历史读取成立：
`SnapshotAtOrBefore` 返回不晚于某序号的最新 tree（`internal/fileplugin/manifest.go:343`），
fork 的覆盖视图按哈希解码父会话的 tree，而不是复制行（`internal/fileplugin/fork.go:18`）。

文件字节由点名它的事件 retain。`Collect` 会删除没有事件引用、且超过保留窗口的对象——默认 24h
（`internal/layerfs/store.go:27`、`internal/layerfs/store.go:502`）。与 blob sweeper 不同，
`keel serve` 目前不调度这条扫描，所以在当前 shell 里请把回收当作维护动作。

```
fs.write（单路径）                     fs.capture（一次结算）
        │                                     │
        ▼                                     ▼
file_manifest upsert                 tree 对象 + session_snapshot(seq, treeHash)
        │                                     │
        └──────────────► layerfs CAS ◄────────┘
                         文件对象、tree 对象
```

## 3. 写入路径：先落账本

写入只有一条路径（`internal/fileplugin/service.go:353`）：内容流进 layerfs → `fs.write` 事件提交 →
之后 `file_manifest` 才变化。若进程死在两步之间，账本仍然持有这次写入；可能缺的只是派生的 manifest
行，接口层会如实报告这个投影缺口，而不是编造文件状态（`internal/fileplugin/service.go:1011`）。
账本永远不是派生物。

每次写入都需要环境里的会话 scope 与 tool-call id；同一调用重放会去重到第一条事件。`expectedHash`
是编辑既有文件的前置条件，创建时必须为空——于是并发改动表现为冲突，而不是静默覆盖。

## 4. Capture：一次结算一个事件

Run 结算会对执行空间做快照，并提交恰好一个 `fs.capture` 事件——失败路径则是一个
`fs.capture_failed`（`internal/fileplugin/service.go:574`）。payload 携带 `treeHash`、`count`、
`newObjects`、`reusedObjects`、`bytes`、`deleted`、`deletedCount` 与 `snapshotVersion`
（`internal/fileplugin/service.go:728`）。

- **幂等。** 结算键是 `callID:baseSeq`；harness 自有的后台任务会加上 `job:` 命名空间，避免 owner
  车道与 system 车道互相去重。
- **删除对账。** 执行空间里已不存在的 manifest 路径会记录在同一条事件的 deleted 列表里。事件文档
  有界：最多 256 条路径 / 32KiB（`internal/fileplugin/service.go:625`）；`deletedCount` 始终报告完整数量。
- **顺序。** 同一会话的显式写入与 capture 是串行的，后提交的 capture 不可能携带更旧的快照。
  提交前崩溃则留下零条 `fs.capture`，重启后的 run 会重新 capture。
- **保留。** capture 会按 `(session, eventSeq)` retain 它点名的每个文件与 tree 对象
  （`internal/fileplugin/service.go:794`），这同时是重放身份。

## 5. 读取是有界分页

文件读取从不「整文件」。默认一页 200 行 / 256KiB，调用方最多可要 2000 行；单行上限 1MiB
（`internal/fileplugin/read.go:13`、`internal/fileplugin/read.go:16`）。分页返回 `totalLines`、
`returnedLines`、`nextOffset` 与 `truncated`，客户端靠偏移续读，而不是重读全文。

面板预览走同一条有界读取：路径必须存在于该会话自己的 manifest，返回的哈希是 manifest 当前哈希，
而不是调用方记忆中的那个（`internal/harnessapi/task_panel.go:117`）。见[会话查询](/zh/core/session-query)。

## 6. Blob：引用、TTL、orphan 扫描

只有当字节写完，blob 才算 complete；引用与点名它的事件在同一个事务内写入
（`docs/BLOB-LIFECYCLE.md:33`）。同一 `(blob, session, eventSeq)` 的重放是 no-op，所以重试提交
不会把计数吹大。

TTL 回收只处理策略标记过的行：`ref_count = 0`、`complete`、`expires_at` 已过期，且没有被别的行的
`thumb_blob_id` 指着（`internal/store/blob_lifecycle.go:93`）。**普通上传本身没有过期时间**——
`SetBlobExpiry` 传 `nil` 表示「永不过期」（`internal/store/blob_lifecycle.go:235`），因此没有
保留策略显式 opt-in 之前，TTL 不会回收任何东西。

删除顺序是严格的（`docs/BLOB-LIFECYCLE.md:92`）：

1. 一个事务内：`blob_refs` → `blobs` 元数据 → `blob_chunks`；
2. 提交；
3. 事务外：先复核 `CASBlobExists(sha256)`，再 unlink CAS 文件（并清理空目录）。

这次复核故意不带租户谓词：CAS 文件按内容跨租户共享（`internal/store/blob_lifecycle.go:248`）。
unlink 失败或崩溃留下的文件由 orphan 扫描重试；扫描只碰 `<2-hex>/<2-hex>/<64-hex>` 且超过宽限期的
文件，永不进入 `layerfs/` 子树（`internal/blob/gc.go:142`）。

| 循环 | 默认 | 作用 |
|---|---|---|
| TTL sweep | `KEEL_BLOB_GC_INTERVAL=1m` | 领取已过期、无引用、已完成的 blob 行（`internal/blob/gc.go:86`） |
| Orphan CAS 扫描 | `KEEL_BLOB_ORPHAN_INTERVAL=30m`，宽限 1h | 重试 unlink 与崩溃残留（`docs/BLOB-LIFECYCLE.md:109`） |
| 缩略图扫描 | `KEEL_BLOB_THUMB_INTERVAL=30s` | 领取 `pending` 缩略图（`cmd/keel/serve.go:984`） |

## 7. 缩略图

图片上传（`jpeg`/`png`/`gif`）走 `pending → ready | failed`；`failed` 是终态，坏图不会被反复重试
（`docs/BLOB-LIFECYCLE.md:130`）。缩略图本身是一条普通 blob 行（`image/png`），因此复用同一套存储
与租户路径。

| 限制 | 值 | 出处 |
|---|---|---|
| 最长边 | 320px，不放大 | `internal/blob/thumb.go:51` |
| 解码像素 | 25 MP | `internal/blob/thumb.go:54` |
| 编码源字节 | 64MiB | `internal/blob/thumb.go:59` |
| 孤儿兜底 | `expires_at = now + 24h` | `internal/blob/thumb.go:61` |

`GET /v1/files/{id}/thumbnail` 返回 `200`（ready，PNG）、`202`（pending）、`404`（该 blob 无缩略图）
或 `409`（failed，或标记 ready 却没有对象）（`internal/httpapi/server.go:1299`）。

## 8. 配额

存储配额按租户、按实测而非缓存：`tenants.quota_storage_bytes` 默认 `0`（= 不限）
（`internal/store/schema.sql:703`）；`AdmitStorage` 在准入前汇总该租户已存字节
（`internal/quota/edge.go:295`）。上传路径由 edge 配额中间件包住，按 `POST /v1/files` 声明的
大小计费（`cmd/keel/edge_quota.go:58`）；超额上传返回 **413**，而流/并发配额返回 **429**
（`internal/quota/edge.go:334`）。

文件插件在下一层执行同一原则：layerfs 在 `Put` 的提交点限制已存字节，超额写入不会留下可见对象
（`internal/layerfs/store.go:234`），工具面报告 `quota_exceeded`（`internal/fileplugin/types.go:34`）。

## 9. 运维操作

上传、查元数据、预览：

```bash
API=http://127.0.0.1:13750
KEY=keel_bot_xxx                        # Bot 绑定的 key；见 API 参考

# 上传（multipart）
curl -sS -X POST "$API/v1/files" -H "Authorization: Bearer $KEY" \
  -F "file=@./diagram.png;type=image/png"

# 元数据 + 缩略图状态
curl -sS "$API/v1/files/<fileId>/meta" -H "Authorization: Bearer $KEY"
curl -sS -o thumb.png -w '%{http_code}\n' \
  "$API/v1/files/<fileId>/thumbnail" -H "Authorization: Bearer $KEY"

# 带 Range 的断点下载
curl -sS -H "Authorization: Bearer $KEY" -H 'Range: bytes=0-65535' \
  "$API/v1/files/<fileId>" -o part.bin
```

在同一个 PostgreSQL 库里查看会话文件视图：

```sql
-- 挂载点：会话有哪些路径，各在哪个 seq 建立
SELECT path, hash, size, updated_seq, last_task_id
  FROM file_manifest WHERE tenant_id = $1 AND session_id = $2
 ORDER BY path LIMIT 20;

-- 快照：某个 seq 时会话看起来是哪棵树
SELECT seq, tree_hash FROM session_snapshot
 WHERE tenant_id = $1 AND session_id = $2 ORDER BY seq DESC LIMIT 5;
```

关键配置：

| 配置 | 作用 | 出处 |
|---|---|---|
| `KEEL_CAS_DIR` | 大对象与 layerfs 的 CAS 根；未设置时退化为临时目录并告警 | `cmd/keel/serve.go:1931` |
| `KEEL_BLOB_GC_INTERVAL` / `KEEL_BLOB_ORPHAN_INTERVAL` | TTL sweep 与 orphan 扫描周期 | `cmd/keel/serve.go:988` |
| `KEEL_BLOB_THUMB_INTERVAL` | 缩略图扫描周期 | `cmd/keel/serve.go:984` |
| `tenants.quota_storage_bytes` | 每租户存储上限，`0` = 不限 | `internal/store/schema.sql:703` |

## 10. 细节在哪里

- [存储与运行](/zh/core/storage)——blob 表、分块决策与背压。
- [会话查询](/zh/core/session-query)——`deliverables`、`file-preview` 及其边界。
- [Provider 适配器](/zh/core/extend/provider-adapter)——出站 `file_id > url > base64` 的解析顺序。
- `docs/BLOB-LIFECYCLE.md`——完整生命周期契约、崩溃点表与测试证据。
- `docs/FILE-PROTOCOL.md`——路径/哈希协议与面向工具的文件动作。
- `internal/pluginpkg/builtin/keel.file.yaml:1`——为 `keel.file.read/write/edit/capture` 提供后端的
  内置插件。
