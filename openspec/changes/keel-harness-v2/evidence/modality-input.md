# 模态输入切片（modality-input）— 证据

> 日期：2026-11-23。Worktree：`.worktrees/keel-modality-input`，branch `task/keel-modality-input`（基于 main `1297837`）。
> Owner 裁决：选中的文本模型若声明支持输入模态，附件必须原生进入请求体（厂商标准形状）；不支持则转
> **blobId/路径 + 元数据文本**，让模型用文件工具读。**永不静默丢弃**。
> 契约文档：[`docs/MODEL-INPUT-MODALITIES.md`](../../../docs/MODEL-INPUT-MODALITIES.md)。

## 1. 交付范围

| 项 | 结果 | 位置 |
|---|---|---|
| 持久附件泛化（非图片 → durable ref） | 实现 | `internal/agentcore/content.go`（`MediaRefContent`）、`internal/agent/messages.go`（`attachmentBlocks`） |
| 请求时模态派发（原生 vs 文本兜底） | 实现 | `internal/agent/attachments.go`（`resolveAttachments`）、runner 每个 attempt 调用 |
| `ModelProperties` 全模态 + 通用能力门 | 实现 | `internal/agent/model.go`（`TextInput/ImageInput/VideoInput/AudioInput/PDFInput`、`AcceptsModality`）、`mediaCapsFor` |
| 协议矩阵（image/pdf/audio/video per API type） | 实现 | `internal/provider/media.go`（`MediaCapsForAPI`、`FileCaps`/`AudioCaps`） |
| 原生编码：OpenAI chat `file` / `input_audio` | 实现 | `internal/provider/providers.go`（`openAIFilePart`、`openAIAudioPart`） |
| 原生编码：OpenAI Responses `input_file` | 实现 | `internal/provider/responses.go`（`responsesFilePart`、`mediaInputParts`） |
| 原生编码：Anthropic `document` | 实现 | `internal/provider/providers.go`（`anthropicDocumentBlock`） |
| 文档/音频 resolver（file_id > url > inline） | 实现 | `internal/provider/media.go`（`ResolveFile`、`ResolveAudio`） |
| 便宜元数据探测（有界头部、零依赖） | 实现 | `internal/provider/mediaprobe.go`（`Probe`、`wavDurationMs`） |
| 二进制预算门 | 实现 | `scripts/binary-size.sh`（`cmd/keel` + `cmd/runtime`）+ `.github/workflows/ci.yml` `binary size gate`（checkout `fetch-depth: 0`） |
| 产品层投影（模态 + MediaCaps + APIType） | 实现 | `internal/servicehost/models.go`、`services.go`（`newMediaResolver`） |
| token 估算覆盖新块 | 实现 | `internal/compaction/tokens.go`（`mediaRefChars`） |
| durable/传输形状测试 | 实现 | `internal/agentcore/content_media_test.go`（JSON 往返 + 分类器） |

未改（按 ownership 约束）：`internal/modelcfg/**`、`internal/harnessapi/**`、`web/**`、
`internal/store/schema.sql`、`cmd/keel/serve.go`、`plugins/**`。

## 2. 门禁命令与结果

```text
$ gofmt -l internal/agent internal/agentcore internal/provider internal/compaction \
           internal/servicehost
(空)

$ go vet ./internal/agent ./internal/agentcore ./internal/provider \
          ./internal/compaction ./internal/servicehost
(exit 0)

$ go build ./...
(exit 0)

$ go test ./internal/agent ./internal/agentcore ./internal/provider \
          ./internal/compaction ./internal/servicehost -count=1
ok  	github.com/jixoai/keel/internal/agent
ok  	github.com/jixoai/keel/internal/agentcore
ok  	github.com/jixoai/keel/internal/provider
ok  	github.com/jixoai/keel/internal/compaction
ok  	github.com/jixoai/keel/internal/servicehost

$ ./scripts/binary-size.sh --base HEAD --budget-kb 128
== binary-size gate ==
base:    HEAD (12978377159ac1412261fe2a185c6688ef7804dd)
budget:  128 KiB per binary and target
targets: linux/amd64 linux/arm64
bins:    ./cmd/keel ./cmd/runtime
keel        linux/amd64      base=33861792 current=33943712 delta=+81920 bytes (+80 KiB)
keel        linux/arm64      base=30933152 current=30998688 delta=+65536 bytes (+64 KiB)
runtime     linux/amd64      base=19943584 current=20021408 delta=+77824 bytes (+76 KiB)
runtime     linux/arm64      base=18415776 current=18481312 delta=+65536 bytes (+64 KiB)
binary-size: OK
```

说明：`scripts/binary-size.sh` 只读 base revision 的 `git archive`，不碰任何工作树；
CI 的 `binary size gate` job 依赖 checkout `fetch-depth: 0`（浅克隆没有 base commit，
脚本现在明确报错而不是静默通过），并同时门禁 release.yml 的 `cmd/keel` 与 Dockerfile 的
`cmd/runtime`。

`internal/harnessapi` 未在本 lane 的必跑集合内（ownership 外），因此没有重复跑它的
`TestMCPAppRejectionAuditSampling`；此前记录的时间采样 flake 与本切片无关。

## 3. 断言清单（测试名 → 钉住的事实）

### agent 派发（`internal/agent/attachments_test.go`）

| 测试 | 事实 |
|---|---|
| `TestAttachmentPDFNativePath` | 模型声明 PDF + 协议可载 → 原生 document 块，resolver 收到协议声明，加载的 transcript 未被改写 |
| `TestAttachmentPDFModelDeniesInput` | 模型否认 PDF → **0 次 resolver 调用**，兜底文本含 name/mime/size/blobId/file tools，且不含字节 |
| `TestAttachmentPDFWireCannotCarry` | 模型声明 PDF 但协议无 document 形状 → 兜底（不伪造部件） |
| `TestAttachmentAudioNativeOnChatWire` | chat + 音频声明 → 原生 inline audio 块 |
| `TestAttachmentAudioFallbackWhereUnsupported` | Responses 协议 → 即使模型声明音频也兜底，原因 “no audio input part” |
| `TestAttachmentVideoAlwaysFallsBack` | 视频在已实现协议下永远兜底，原因 “no provider protocol” |
| `TestAttachmentResolverFailureLeavesText` | 上传失败（401）→ 兜底含原因，用户 prompt 不丢 |
| `TestAttachmentProbeFailureKeepsReferenceFacts` | 探测失败 → 兜底仍含引用自带 name/mime/size/blobId |
| `TestAttachmentWithoutBlobIdIsNotDropped` | 客户端未给 fileId → 重建即显式 notice |
| `TestAttachmentNoResolverStillNamesTheBlob` | 无 resolver 部署 → 全部附件兜底 + 一次 degraded 报告 |
| `TestAttachmentTextOnlyModelReceivesOnlyMetadata` | 纯文本模型 → 每个附件一个文本块，0 次 resolver |
| `TestMediaCapsForGatesEveryModality` | 通用能力门：逐模态归零，nil 属性放行 |
| `TestMediaCapsForKeepsTheLegacyImageField` | 旧 `ImageCaps` 字段仍生效（向后兼容） |
| `TestImageReferenceOnATextOnlyModelDegrades` | 图片旧路径同样受通用门约束：不调用 resolver、文本兜底 |
| `TestUserMessageAttachmentsDecodeAsDurableReferences` | 每个附件一 ref（imageRef/mediaRef），无字节；无 fileId → notice |
| `TestNonImageAttachmentsBecomeDurableReferences` | 非图片附件进入 transcript（不再静默丢弃） |
| `TestAttachmentWithoutMimeButImageExtensionIsAnImage` | MIME 缺失但扩展名是图片 → imageRef（不把图片降级成文档） |
| `TestAttachmentNoResolverBlamesThePipelineNotTheWire` | 无 resolver 时原因是 wiring 缺失，不甩锅给支持 PDF 的协议 |
| `TestAttachmentAudioResolverFailureFallsBack` | 音频 resolver 失败 → 文本兜底，不伪造原生音频块 |
| `TestAttachmentWithoutBlobIdFallsBackHonestly` | 空 blobId 的 ref → 不探测、不声称存在已存字节、仍提示 file tools |
| `TestAttachmentMixedNativeAndFallbackPreservesOrder` | 同一条消息里原生 document + 兜底 audio 保持客户端顺序；transcript 不被改写 |
| `TestAttachmentVideoModelDeniesInput` | 视频：模型否认时给模型原因（先于协议行） |
| `TestAttachmentFallbackStatesUnknowns` | 兜底 label 对 pages/duration/dimensions 写 unknown；已知值写实数 |
| `TestTrimReasonCutsOnARuneBoundary` | 超长原因是合法 UTF-8、有界、带截断标记 |

### 持久/传输形状（`internal/agentcore/content_media_test.go`）

| 测试 | 事实 |
|---|---|
| `TestMediaReferenceRoundTrips` | mediaRef 的全部字段经事件日志 JSON 往返不丢 |
| `TestTransportMediaBlocksDecode` | file/audio 传输块在封闭内容词表内，回放不因未知类型失败 |
| `TestInputKindForMime` | 分类器：PDF/音频/视频/通用文件；图片 MIME 归通用文件（图片走 imageRef） |

### token 估算（`internal/compaction/conformance_test.go`）

| 测试 | 事实 |
|---|---|
| `TestConformanceTokenEstimation/counts a durable attachment reference as a bounded text block` | mediaRef 按 `mediaRefChars` 计价，不是免费 |
| `.../counts wire-only document and audio blocks defensively` | 手工构造的 file/audio 块同样计价 |
| `.../counts a media reference alongside text` | 文本 + mediaRef 的合计按 ceil(chars/3.5) |

### provider wire / resolver（`internal/provider/media_test.go`）

| 测试 | 事实 |
|---|---|
| `TestOpenAIChatWireCarriesNativePDF` | body：`file` 部件、`file_data`=data URL、`filename`；无并存 `file_id` |
| `TestOpenAIChatWireCarriesNativePDFByFileID` | body：`file` 部件、`file_id`；无并存 `file_data` |
| `TestOpenAIChatWireCarriesNativeImage` | 重构后图片仍为 `image_url` data URI，prompt 保留 |
| `TestOpenAIChatWireCarriesNativeAudio` | body：`input_audio` 的 `data`+`format` |
| `TestOpenAIChatRefusesADocumentTheWireCannotCarry` | 非 PDF / url 形状 → body 内显式 “document omitted” |
| `TestResponsesWireCarriesNativePDF` | body：`input_file` 的 `file_id` / `file_data`(+filename) / `file_url` 三形态 |
| `TestResponsesWireNeverSendsInputAudio` | body 无 `"type":"input_audio"`，只有显式 audio notice |
| `TestAnthropicWireCarriesNativePDF` | body：`document` 的 base64 / url source |
| `TestAnthropicWireNeverSendsAudio` | body 只有显式 audio notice |
| `TestNativePDFReachesAMockProviderEndToEnd` | httptest 厂商收到 inline PDF（编码+传输+鉴权全链） |
| `TestMediaKindAndAudioFormatClassification` | MIME/扩展名分类器与 agentcore 版本一致 |
| `TestMediaCapsMatrix` | 协议矩阵（含 responses 无 audio、anthropic 无 file_id/audio/video、未知协议空声明） |
| `TestResolveFileForms` | inline 形态正确；无形状/无 MIME 拒绝并命名原因 |
| `TestResolveFileMissingBlobFailsClosed` | blob 缺失 → 拒绝（不臆造 base64） |
| `TestResolveFileWithoutBlobIDFailsClosed` | 无 blobId → 拒绝 |
| `TestResolveAudioInlineOnly` | wav 内联；flac 拒绝；无音频部件的协议拒绝 |
| `TestResolveFilePrefersFileHandleWhenDeclared` | file_id > inline，内联路径未跑 |
| `TestProbeReadsOnlyBoundedMetadata` | stdlib 解 PNG 尺寸；PDF pages=unknown；WAV duration=1000ms |
| `TestProbeReadsOnlyABoundedHeader` | 计数 blob 源：一次 open、恰好 256KiB 头部读取、reader 关闭 |
| `TestProbeUsesClientDimensionsWithoutOpeningTheBlob` | 客户端已给尺寸 → 0 次 open |
| `TestProbeLeavesVideoAndUnsupportedImagesUnknown` | MP4 尺寸/时长、WebP 尺寸都写 unknown，不猜 |
| `TestProbeCompressedWavStaysUnknown` | 非 PCM/float WAV 不按 byte-rate 算时长（写 unknown） |
| `TestAnthropicWireRefusesDocumentFormsItCannotCarry` | anthropic `document`：file_id（需 beta 头）与非 PDF MIME → 显式 notice |
| `TestOpenAIChatAudioRefusalNotices` | chat `input_audio`：空 data、非 wav/mp3 → 显式 notice |
| `TestOpenAIDocumentRefusesAmbiguousSources` | 文档 0 个或 2 个 source → 显式 notice（不产出空 file 部件） |

### 装配态端到端（`internal/servicehost/modality_test.go`）

| 测试 | 事实 |
|---|---|
| `TestAssembledRuntimeCarriesANativePDF` | Boot → runner → 真 resolver → chat 编码：厂商 body 含 data-URL PDF |
| `TestAssembledRuntimeDegradesPDFOnATextOnlyModel` | 同一链路，纯文本模型：body 无 `file` 部件，含 name/mime/blobId/原因 |
| `TestAssembledRuntimeModelPropertiesCarryEveryModality` | 解析身份带全模态、APIType、MediaCaps，旧 ImageCaps 镜像 |
| `TestAssembledRuntimeCarriesNativeAudio` | Boot → 真 resolver → chat 编码：厂商 body 含 `input_audio`（wav + blob base64） |
| `TestAssembledRuntimeUndeclaredModalityDegrades` | 模型文档未声明 inputFormat → 同一链路走文本兜底（目录通配基线 text-only 的后果） |
| `TestMediaCapsForAPIUnknownDeclaresNothing` | `demo` 协议空声明 |

## 4. 协议矩阵（已实现的原生形状）

| 协议 | image | pdf | audio | video |
|---|---|---|---|---|
| `openai-chat-completions` | `image_url` | `file`（`file_data`+`filename` / `file_id`，PDF） | `input_audio`（wav/mp3） | — |
| `openai-responses` | `input_image` | `input_file`（`file_id`/`file_url`/`file_data`） | — | — |
| `anthropic-messages` | `image`（base64/url/file） | `document`（base64/url） | — | — |
| 未知 / `demo` | 仅 base64 | — | — | — |

## 5. 剩余 / 明确不承载

- **audio @ Responses**：上游拒绝 `input_audio`；兜底文本（OpenAI 文档音频输入指向 Chat Completions）。
- **video @ 全部已实现协议**：无原生部件；兜底。Gemini 原生视频需单独 generativelanguage 适配器，未做。
- **pdf `file_id` @ Anthropic Messages**：需消息级 Files beta 头（本驱动不发）；编码器把该形状显式转成 text notice，resolver 也从不产出它（用 url/inline 或兜底）。
- **非 PDF 文档**：适配器只编码 PDF；docx/xlsx 等兜底文本（编码器显式 notice，不伪造部件）。
- **PDF 页数、MP4/WebP/HEIC 元数据**：按 Owner 约束不引入解析器/依赖；兜底写 `unknown`。
- **模型元数据编辑 UI**：归 model-catalog lane，本切片未改 `web/**`；该 lane 需要为视觉/PDF/音频模型声明 `inputFormat`（内置目录通配规则目前是 text-only，未声明的模型一律文本兜底）。
- **`GET /v1/models` 的 `enforcement.media`**：仍保守返回 false；归 models API lane 与已实装行为对齐。

## 6. 自查（review 记录）

- 每个受影响文件逐行审阅：`git diff 1297837 -- internal/agent internal/agentcore internal/provider internal/compaction internal/servicehost scripts/binary-size.sh .github/workflows/ci.yml docs/MODEL-INPUT-MODALITIES.md docs/REAL-MODEL-E2E.md`。
- 无凭据/字节进入日志：兜底文本只含 blobId/name/mime/size；`logAttachmentResolution` 只记 blobId/kind/reason。
- 无新 Go module（`go.mod`/`go.sum` 未改）；四个测量点最大增量 +80 KiB（keel/amd64），低于 128 KiB 预算。
- 兜底原因真实：resolver 缺失时不再甩锅协议；anthropic 非 PDF/file_id 形状、chat 非 PDF/坏音频、文档 source 数量异常都显式 notice，不伪造部件。
- 无持久进程：全部验证为单次 `go test`/`go vet`/`go build`/`scripts/binary-size.sh`，未启动 daemon。
- final tree 只含本 lane 的预期文件（见 §1）。
