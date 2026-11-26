# Design: keel-plugin-attachment

## 1. 定位与三层事实

附件不是一个新领域对象，而是既有 blob 在会话里的**引用视图**。事实仍分三层，本 change
不改变任何一层的所有权：

| 层 | 事实 | 代码/契约 |
|---|---|---|
| 持久层 | `user.message.attachments[]` 每项 → 一个 durable ref：图片 `ImageRefContent`，其余 `MediaRefContent`（`kind`/`blobId`/`name`/`mime`/`size`/可选尺寸）。事件日志永不含字节。 | `internal/agent/messages.go`、`internal/agentcore/content.go` |
| 模型声明 | `modelcfg.Properties.InputFormat` → `agent.ModelProperties`；每次 attempt 投影一次。 | `internal/servicehost/models.go`、`internal/agent/model.go` |
| 协议能力 | `provider.MediaCapsForAPI(apiType)`；只有适配器真正实现的形状才声明为真。 | `internal/provider/media.go` |

插件只消费**第一层的元数据投影**，不拥有第二、三层。事件日志仍是附件引用的唯一真值；
预览/渲染/缩略图都不追加会话事件，也不改变 `user.message` 的 payload。

## 2. 唯一生命周期

```text
POST /v1/files ──> blob（tenant_id, blob_id, complete, mime, size, thumb_status）
      │                          │
      │ client 拿到 fileId       ├─ 缩略图（异步，独立 blob）
      ▼                          │
POST /v1/sessions/{id}/input     │
  attachments[] {fileId,name,mime,size,width?,height?}
      │ 输入期校验（本 change）
      ▼
user.message.attachments[] ──> blob_refs（引用计数；事件事务内）
      │
      ▼
attempt：模型声明 + 协议能力
      ├─ 原生 wire 部件（image/file/audio）
      └─ 文本兜底（blobId + name/mime/size + 原因；无字节）
      │
      ▼
GC：ref_count=0 且过期 → metadata → chunks/CAS
```

复用点（MUST NOT 另建）：

- 上传与下载：`POST /v1/files`、`GET /v1/files/{id}`、`/meta`、`/thumbnail`；
- 持久引用：`blob_refs` 与 `internal/kernel/blobrefs.go`；
- 模型派发：`internal/agent/attachments.go`、`internal/agent/images.go`；
- 缩略图与回收：`internal/blob/{thumb,gc}.go`。

## 3. 输入期校验（新增）

`POST /v1/sessions/{id}/input` 在提交 `user.message` 之前校验附件数组；任何失败都**不落事件**。

| 规则 | 值/判定 | 失败 |
|---|---|---|
| 条数 | ≤16/输入（与 MCP Apps 的 `mcpAppMaxAttachments` 同口径） | `400 attachments` |
| 单文件 | ≤`MaxUpload`（默认 64 MiB）与租户存储配额 | `413`/`429` |
| 名称 | 1..256 字节；只展示，不参与路径 | `400 attachments[i].name` |
| MIME | ≤128 字节；小写归一去参数后与 blob 记录比较 | `400 attachments[i].mime` |
| 归属 | `fileId` 必须属于当前 tenant 且 `complete=true` | `400 attachments[i].fileId`（不区分不存在/跨租户） |
| 形状 | 只允许 `fileId/name/mime/size/width/height`；未知字段忽略 | — |
| 缺 `fileId` | 保留现有语义：生成显式文本 notice，模型知道附件存在但不可用 | 不拒绝整条输入 |

校验放在共享提交路径（`CommitClientInput`）而不是只放在 HTTP handler，MCP Apps 的
`ui/message` 与 retry 复用同一规则，避免旁路。retry 按原始 payload 重新校验；旧会话中
不合规的附件不会因 retry 被放行，也不做历史数据迁移（Alpha 零兼容）。

## 4. 插件可见的附件投影

宿主只发布一个只读文档（命名可为 `AttachmentPart`）：

```ts
type AttachmentPart = {
  fileId: string;                 // 当前租户的 blob id
  name: string;                   // 展示名，已做长度限制；不可当路径
  mime: string;                   // blob 记录的权威 MIME
  size: number;
  kind: "image" | "pdf" | "audio" | "video" | "file";
  width?: number; height?: number;
  thumbStatus: "pending" | "ready" | "failed" | "none";
  thumbnailUrl?: string;          // 同源 /v1/files/{id}/thumbnail
  downloadUrl: string;            // 同源 /v1/files/{id}
  delivery: {
    native: boolean;              // 本次 attempt 是否原生投递
    reason?: string;              // 非原生时的稳定原因码/短语
  };
};
```

硬约束：

- 不含字节、base64、data URI、CAS 路径、`X-Tenant-ID`、cookie 或任何凭据；
- URL 是同源、受当前 principal 与租户鉴权的普通路由，不是跨租户签名 URL；
- 后端插件（sidecar/tool）在本 change **拿不到字节句柄**；需要读字节的能力另立 change，
  并复用租户 blob 边界与 `file.read`；
- 投影按事件 seq 与 attempt 结果生成，插件不能用它反推别的会话。

## 5. 预览/渲染槽位

复用 `keel.ui.message-part`（声明者 `keel.ui`，form 允许 `chunk`/`iframe`，target 含
web/native/tui）：

```yaml
contributes:
  - slot: keel.ui.message-part
    form: chunk
    entry: ui/pdf-preview.js
```

`extensibility.json` 的同一条贡献声明认领范围：

```json
{ "slot": "keel.ui.message-part", "form": "chunk",
  "entry": "ui/pdf-preview.js", "part": "attachment",
  "mime": ["application/pdf", "image/*"] }
```

选择规则：

1. 只考虑 `part=attachment` 且 `status=ready`、target 可用的贡献；
2. 有 `mime` 时按声明顺序取第一个匹配；无 `mime` 视为通配；
3. 没有匹配或渲染抛错 → 宿主默认附件行（文件名/大小/MIME/缩略图/下载），并显示可解释的
   失败/降级提示；
4. 渲染器是**增量**：原始文件名、大小与下载/打开入口 MUST 仍可见；渲染器不得把附件
   变成不可下载的纯视觉块；
5. 主题走宿主令牌；`iframe` 显式标注“非原生主题”；TUI 对 iframe 显示不可用与在图形界面
   打开的入口，`chunk` 按 TUI 视图渲染。

失败隔离：单个贡献失败只影响该附件行；同一会话其他附件、其他插件与整轮对话不受影响。

认领信息（`part=attachment`、`mime`）随 `extensibility.json` 下发；这是对
`keel.plugin-extensibility/1` 贡献条目的向后兼容扩展（新增可选字段），未知字段拒绝规则
要求实现同步更新 schema 校验。解码时必须按 `(slot, form, entry)` 把 `part/mime` 合并到
`plugin.yaml` 已声明的同一条贡献，MUST NOT 产生重复条目。未提供 `extensibility.json` 的
包不能被识别为附件渲染器，但仍可向 `keel.ui.message-part` 投普通消息部件。

## 6. 模型派发与兜底（复用，不重写）

```text
durable ref
  ├ 模型声明 denies 该模态        → 文本兜底（不上传、不内联）
  ├ 协议无原生形状                → 文本兜底
  ├ resolver 失败（blob 缺失/401）→ 文本兜底，带原因
  └ 否则                          → 原生 wire 块
```

文本兜底保留 `blobId`、name/mime/size 与未知项 `unknown`，让模型可用文件工具继续读；
“任何情况下不得静默丢弃附件”仍是硬约束。`delivery.native/reason` 投影的就是这条判定，
插件不自行判断“模型能不能看”。

## 7. 租户、权限与审计

- **租户**：所有附件路由与投影带 `tenant_id`；跨租户 `fileId` 在输入期即失败，读取期
  返回 404，不泄露存在性。缩略图沿用父 blob 的租户谓词。会话投影另受 `session.read`
  关系与归属约束。
- **权限**：预览入口是会话读取的一部分（`session.read` 关系 + 会话归属）；插件安装审批
  展示“将认领 `keel.ui.message-part` 的 attachment 贡献 + MIME 列表”；插件工具被预览调用时
  仍要 `plugin.invoke` 与 `permissions.tools`，越权写 `plugin_audit`。
- **审计**：附件输入/降级不逐条写审计（噪声且含元数据），但以下必须留痕：插件安装/启用/
  停用/卸载、插件越权拒绝、预览贡献装载失败（插件诊断）。审计 detail 只含 pluginId、
  fileId（或哈希）、mime、size、结果，不含文件名、正文、字节或 URL 凭据。
- **日志**：附件解析降级逐条 info/warn，含 `fileId/size/mime/reason`；不记录 name 与内容。

## 8. 失败模式与清理

| 情形 | 结果 | 清理/恢复 |
|---|---|---|
| 上传未完成（`complete=false`） | 输入 400；不落事件 | 重新上传后重试 |
| 跨租户/未知 fileId | 输入 400（同一字段错误） | 无副作用 |
| 声明 MIME 与 blob 不一致 | 输入 400 | 无副作用 |
| 模型/协议不支持 | 文本兜底 + 原因 | 下一 attempt 可重新判定 |
| blob 缺失/上传 401 | 文本兜底 + 原因 | 修配置后重试 |
| 缩略图 failed | 预览显示无缩略图，占位仍可下载 | 终态，不重试风暴 |
| 插件渲染失败 | 默认附件行 + 失败提示 | 停用/重载插件即恢复 |
| 插件停用/卸载 | 贡献在同一 generation 撤销 | 无残留订阅/缓存；临时预览对象由客户端释放 |
| 引用归零且过期 | TTL GC 删 metadata → chunks/CAS | 幂等，重复 sweep 安全 |

## 9. 与 file/media 工具的关系

- `read`/`write`/`edit` 操作**会话 layerfs 工作区**，不是 blob 附件；附件不是工作区文件，
  不因 `read` 出现在文件 manifest。
- `mediaadapter` 的 `image_generate`/`video_generate`/`speech_*` 是**生成器**，产物可再被
  上传为附件，但不共享附件输入语义。
- 插件预览只读投影；任何“插件生成预览并写回 blob”的能力不在本 change，避免第二套缓存。

## 10. 非目标

- 插件后端附件字节读取、服务端转码/OCR/PDF 抽页/视频抽帧；
- 签名 URL、跨租户共享、附件编辑/版本；
- 新上传路由、新 blob 表、新引用计数；
- 把 `keel.ui.message-part` 变成附件专属槽位（它仍是通用消息部件槽位）。

## 11. 已登记的后续契约（本切片不修）

- **跨插件优先序**：`GET /v1/plugins` 的 inventory 顺序即选择优先序。当前实现按 plugin id
  排序返回插件；同一插件内按 `contributes[]` 声明顺序。UI 3.2 必须按该顺序取第一个 ready
  且 MIME 匹配的贡献，不得自行重排。若要改成“安装时间/显式 priority”，必须先改 inventory
  契约与前端，而不是在预览层另建排序。
- **blob 路由能力模型**：`/v1/files/{id}`、`/thumbnail` 与 `/v1/attachments/{id}/part` 使用
  tenant + `file.read`（`access.KeyFileRead`），不叠加 `session.read`；调用方必须是该 tenant
  的已认证 principal。会话视图的可见性仍由 `session.read` 的会话面约束；跨租户/无权限的
  端到端行为由 5.5 E2E 覆盖。若未来要求“附件 URL 也只能从可读会话访问”，需要新增
  session-scoped 路由或带 session 谓词的签名入口，不在本 change 内偷偷加。

