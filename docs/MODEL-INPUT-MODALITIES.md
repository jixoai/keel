# 模型输入模态：原生优先，文本兜底，永不静默丢弃

> 变更：`keel-harness-v2` · modality-input 切片 · 2026-11-23
> Owner 裁决（2026-11-23）：**LLM 输入模态是最大变量**。选中的文本模型若声明支持某输入
> 模态，附件必须以该厂商的标准形状**原生进入请求体**；不支持时，把附件转成
> **路径/blobId + 元数据的文本块**，让模型用文件工具去读。**任何情况下不得静默丢弃附件**。
>
> 同一裁决的二进制约束：元数据必须便宜、零依赖、只读有界头部；PDF 页数、音视频时长/尺寸
> 在便宜拿不到时明确写 `unknown`，不许猜；新增依赖必须先过 `scripts/binary-size.sh` 的预算门。

## 1. 三层事实（谁说了算）

| 层 | 事实 | 代码 |
|---|---|---|
| 持久层 | `user.message` 的 `attachments[]` 每项 → 一个 durable ref：图片 `agentcore.ImageRefContent`（MIME 缺失时按图片扩展名判定），其余 `agentcore.MediaRefContent`（`kind`/`blobId`/`name`/`mime`/`size`/可选 `width`/`height`）。事件日志永不含字节。 | `internal/agent/messages.go`、`internal/agentcore/content.go` |
| 模型声明 | `modelcfg.Properties.InputFormat`（text/image/video/audio/pdf）→ `agent.ModelProperties`（同名字段）。**产品层每次尝试都会投影一个声明**：模型文档没写 `inputFormat` 时继承其规则基线（内置目录的通配规则声明 text-only），所以“没声明”＝不原生接收，附件走文本兜底；操作方/模型目录 lane 必须为视觉、PDF、音频模型逐条声明。`nil` 只出现在宿主或测试直接构造时，表示未知（全部放行）。 | `internal/servicehost/models.go`、`internal/agent/model.go` |
| 协议能力 | `provider.MediaCapsForAPI(apiType)`：图像/文档/音频/视频各自的线形状与上限。**适配器真正实现的才声明为真**。 | `internal/provider/media.go` |

## 2. 协议矩阵（native 形状）

| 协议 (`api.type`) | image | pdf/document | audio | video |
|---|---|---|---|---|
| `openai-chat-completions` | `image_url`（URL 或 data URI） | `file` 部件：`file_data`（data URL）+ `filename`，或 `file_id`；**仅 PDF** | `input_audio`（base64 + `format` wav/mp3） | — |
| `openai-responses` | `input_image`（`image_url` 或 `file_id`） | `input_file`：`file_id` / `file_url` / `file_data`（data URL + `filename`） | —（见 §4） | — |
| `anthropic-messages` | `image`（`source` base64/url；**无** `file`，消息请求不带 Files API beta 头） | `document`（`source` base64/url；`file_id` 形状被编码器显式拒绝成文本 notice） | — | — |
| 未知协议 / `demo` | 仅 base64 | — | — | — |

线形状判定代码：`internal/provider/providers.go`（chat / anthropic）、
`internal/provider/responses.go`（Responses）。表格由
`TestMediaCapsMatrix` 钉住（`internal/provider/media_test.go`）。

## 3. 派发顺序（每个附件、每次 attempt）

```
durable ref
  ├ kind = pdf/audio/video/file（producer 的 Kind，否则 MIME+扩展名）
  ├ 模型声明 denies 该模态            → 文本兜底（不触发上传、不内联）
  ├ 协议无该模态的原生形状            → 文本兜底
  ├ resolver 失败（blob 缺失/上传 401）→ 文本兜底，带原因
  └ 否则                              → 原生 wire 块（file/audio/image）
```

- 图片：`internal/agent/images.go`（原路径保留；能力门改为通用门 `mediaCapsFor`，模型
  明确否认图片输入时同样走文本兜底）。
- 其余：`internal/agent/attachments.go`。
- 文本兜底内容：`[attachment <name>, <mime>, <size>, <pages unknown|dimensions unknown|duration unknown> ... was not sent with this request: <reason>. Stored bytes are referenced by blobId=<id> (platform file id; the tenant file endpoint GET /v1/files/<id> streams the bytes). Use file tools to inspect the attachment if you need its contents.]`
  —— 无字节、无 base64、无凭据；未知项显式写 `unknown`。

## 4. 已知无法原生承载的模态（明确剩余）

| 模态 | 现状 | 原因 |
|---|---|---|
| audio @ `openai-responses` | 文本兜底 | `input_audio` 在 SDK 类型里有，但上游运行时拒绝；OpenAI 官方音频输入文档指向 Chat Completions。声明为真会伪造上游不接受的部件。 |
| video @ 所有协议 | 文本兜底 | 本包实现的三种协议都没有 video 输入部件（Gemini 原生视频需要单独的 generativelanguage 适配器，本仓 `google` 走 OpenAI-compatible 线）。 |
| pdf `file_id` @ `anthropic-messages` | 文本兜底或 url/inline | `document.source.type=file` 需要消息请求带 Files API beta 头，本驱动不发；编码器现在把该形状显式转成 text notice（`TestAnthropicWireRefusesDocumentFormsItCannotCarry` 钉住），绝不伪造部件。 |
| 文档非 PDF（docx/xlsx/…） | 文本兜底 | 适配器只编码 PDF 形状；`text/*` 之外的格式上游支持面未在本仓验证，编码器同样显式拒绝成 text notice。 |

## 5. 元数据（便宜、零依赖、有界）

`provider.MediaResolver.Probe`（`internal/provider/mediaprobe.go`）：

| 事实 | 来源 | 依赖 |
|---|---|---|
| blobId / size / mime | `BlobSource.Info`（blob 行元数据） | 无 |
| 图片 width/height | 客户端已给则零 I/O；否则 `image.DecodeConfig`，只读 ≤256KiB 头部 | stdlib `image/png`、`image/jpeg`、`image/gif` |
| WAV duration | RIFF/`fmt `/`data` 头（PCM/float 才计算） | stdlib `encoding/binary` |
| PDF pages | **不解析**：写 `pages unknown` | 无（不为元数据引入 MB 级解析器） |
| MP4/MOV 尺寸/时长 | **不解析**：写 `unknown` | 无 |
| WebP/HEIC 尺寸 | 未知（stdlib 无解码器） | 不引入 `x/image`；按需再加并过预算门 |

只读有界头部（`mediaHeaderBytes = 256KiB`），绝不整文件进内存；header 读取失败不影响
兜底文本（引用自带的事实仍在）。`TestProbeReadsOnlyABoundedHeader` 用计数 blob 源钉住
「一次 open、≤256KiB 读取」。

## 6. 二进制预算门

`scripts/binary-size.sh`：对 base revision 与工作树各构建一次
（`CGO_ENABLED=0`、`-trimpath -ldflags '-s -w'`、linux/amd64 + linux/arm64），
逐个 **出货二进制** 测量：`cmd/keel`（release.yml 发布的 CLI）与 `cmd/runtime`
（Dockerfile 的服务端镜像）。默认预算 128 KiB/二进制/目标，超预算失败；CI 的
`binary size gate` job 跑它（PR 用 base sha，push 用 `github.event.before`），
因此 checkout 必须是 `fetch-depth: 0`（浅克隆没有 base commit，脚本会明确报错而不是
静默通过）。

本切片实测（未新增任何 Go module；base `1297837`）：

| 二进制 | 目标 | base | after | delta |
|---|---|---:|---:|---:|
| `cmd/keel` | linux/amd64 | 33,861,792 | 33,943,712 | +81,920 B (+80 KiB, +0.24%) |
| `cmd/keel` | linux/arm64 | 30,933,152 | 30,998,688 | +65,536 B (+64 KiB, +0.21%) |
| `cmd/runtime` | linux/amd64 | 19,943,584 | 20,021,408 | +77,824 B (+76 KiB, +0.39%) |
| `cmd/runtime` | linux/arm64 | 18,415,776 | 18,481,312 | +65,536 B (+64 KiB, +0.36%) |

增量来自新增的兜底/解析代码与 stdlib 图像头解码器（`image/png|jpeg|gif`），远低于
128 KiB 预算；没有引入 `x/image`、PDF 解析器、ffmpeg 或 cgo。

## 7. 失败语义汇总

| 情形 | 结果 |
|---|---|
| 附件无 `fileId`（客户端只发了 name/mime） | 重建时立即成 `[attachment … omitted: the client sent no fileId …]` 文本块 |
| `fileId` 不属于当前租户 / 未完成 / 不存在 | 输入期字段级 400，不落事件（读取期 404，不泄露存在性） |
| 声明 MIME 与 blob 记录不一致（归一后） | 输入期字段级 400，不落事件 |
| 超过 16 个附件 / 单文件超过 `MaxUpload` | 输入期 400 / 413，不落事件 |
| blob 不存在 / 无 workspace | 文本兜底，原因含 blobId 与错误 |
| 模型声明不支持该模态 | 文本兜底，原因 `the selected model does not declare …` |
| 协议无原生形状 | 文本兜底，原因 `… wire protocol cannot carry …` |
| 上传/内联失败（401、超限） | 文本兜底，原因含厂商错误；其余附件照常 |
| 无 resolver 的部署 | 全部附件→文本兜底（引用自带事实），日志 warn 一次 |
| header 探测失败 | 兜底文本仍含引用事实（name/mime/size） |

日志：`logAttachmentResolution`（失败/降级 warn；文本兜底逐条 info 并带
`blobId/size/mime/reason`，不记 name/正文/字节）；`logImageResolution` 保留（图片文本
兜底也记录同一组字段）。

## 8. 插件附件面（change `keel-plugin-attachment`）

插件只读附件元数据，不拥有附件生命周期；字节与引用计数仍归宿主。

**唯一生命周期**（MUST NOT 另建第二套）：

```text
POST /v1/files → blob（tenant, complete, mime, size, thumb_status）
  → 输入期校验（数量/大小/MIME/归属/完成态）
  → user.message.attachments[]（blobId + 展示元数据）
  → blob_refs（引用计数/TTL，事件事务内）
  → attempt：原生 wire 部件 或 文本兜底
```

**输入期校验**（`POST /v1/sessions/{id}/input`、MCP Apps `ui/message`、retry 共用
`CommitClientInput`）：≤16 个附件；每个只允许 `fileId/name/mime/size/width/height`
（未知字段忽略）；`fileId` 属于当前 tenant 且 `complete=true`；声明 MIME 以 blob 记录为
权威（小写去参数后比较）；name 1..256 字节且只作展示；失败返回字段级错误且不落事件。
缺 `fileId` 是唯一不拒绝的降级：生成显式文本 notice，模型仍知道附件存在。

**只读投影**（`AttachmentPart`，后端为 `GET /v1/plugins` 的 contribution 投影提供
`part/mime` 认领）：`fileId/name/mime/size/kind`、可选尺寸、`thumbStatus`、同源
`thumbnailUrl/downloadUrl`、本次 attempt 的 `delivery.native/reason`。MUST NOT 含字节、
base64、data URI、CAS 路径、租户头、cookie、凭据或跨租户 URL；插件后端在本 change
**拿不到字节句柄**。

**预览槽位**：复用 `keel.ui.message-part`，贡献以 `part=attachment` 认领，可在
`extensibility.json` 用 `mime` 收窄（`type/subtype` 或 `type/*`）。宿主按声明顺序选择
第一个 ready、target 可用且 MIME 匹配的贡献（先声明先匹配）；没有匹配或渲染失败时回退
宿主默认附件行。渲染器 MUST 保留文件名、大小与下载/打开入口；TUI 的 `iframe` 贡献显式
显示不可用。

交叉引用：[`BLOB-LIFECYCLE.md`](BLOB-LIFECYCLE.md) 负责 blob 存储、引用计数、TTL 与
缩略图；本文件负责模型输入与插件投影。插件契约细节见
[`PLUGIN-EXTENSIBILITY.md`](PLUGIN-EXTENSIBILITY.md)。

## 9. 非目标（本切片）

- 不做 PDF 页数解析、视频抽帧、音频转写（那需要外部进程或大依赖）。
- 不改 `web/**`（模型元数据编辑归 model-catalog lane）。
- 不实现 Gemini 原生 `generativelanguage` 适配器（`google` 仍走 OpenAI-compatible 线；
  一旦加适配器，在 `MediaCapsForAPI` 里声明 video/file_uri 并补请求体测试）。
- 不给插件后端附件字节读取、签名 URL、服务端转码/OCR/PDF 抽页或预览缓存服务；这些
  属于后续独立 change。
