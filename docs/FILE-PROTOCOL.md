# 文件协议：先上传，后引用

> 来源：2026-11-05 pi 生态 / 各厂商 Files API 调查。结论：**pi / pi-durable / pigo 全是
> base64-only，没有 file 协议**；「上传一次、多轮引用」必须由本仓自建 blob 层 + provider adapter。

## 1. 现状事实（带源码锚点）

### pi 生态

- `pi-ai/packages/ai/src/types.ts:446-450`：`ImageContent = {type:"image", data:<base64>, mimeType}`，
  没有 `url`/`file_id`/`file_uri`。
- OpenAI Chat：`api/openai-completions.ts:1276,1436` 用户图与 tool 图都拼 `data:<mime>;base64,...`；
  即使服务端支持 https URL 也不透传。
- OpenAI Responses：`api/openai-responses-shared.ts:106,317` 固定 `input_image.image_url=data:...`，
  未用 `file_id`。
- Anthropic：`api/anthropic-messages.ts:143-151` 固定 `source.type="base64"`；
  Google：`api/google-shared.ts:217,305-310` 固定 `inlineData`；
  Bedrock：`api/bedrock-converse-stream.ts:1005,1422` 只发 `bytes`（原生 `s3Location` 未用）。
- 模型能力只判断 `input.includes("image")`，没有「是否支持 URL/file_id」的传输能力位。
- pi-durable `read` 把图片作为 `ImageContent` 返回（`packages/durable/src/tools/read.ts:147-200`），
  tool result 原样进 transcript（`harness/tool.ts:1068-1092`）；README:249/279 明确是 base64。
- pi issue #9613 实证：session JSONL 内联 base64，单文件 2.6GB、6739 张图、resume OOM；
  结论「no externalization/lazy-loading」，closed no-action。issue #8617 才提出 outbound
  `file_id`，仍 open。
- pigo 同样：`internal/agentcore/content.go:61-63` 只有 data+mimeType；session JSONL 直接序列化
  消息（`internal/session/session.go:102-129`）。
- pigo 的 `@image:`：`internal/cli/ui/imageref.go:28-100` 读本地文件→base64；粘贴走
  `internal/cli/tui/clipimage.go:16-58` 存临时 PNG→composer `[Image #N]`→提交时
  `model.go:1149-1164` 展开成 `@image:<path>`。每轮都重新读盘+base64。
- pigo tool result 图片未实现：`internal/provider/providers.go:200-240,475-505` 的 tool 消息用
  `ContentToText` 抹平；`read_tool.go` 仅文本。只有 user 图片链路。

### 厂商协议

| 厂商 | 上传 | 图片引用形态 | 限制 | 备注 |
|---|---|---|---|---|
| OpenAI | `POST /v1/files`（purpose `vision`/`user_data`） | Responses `input_image` 支持 `image_url` **或 `file_id`**；Chat 的 `image_url` 不支持 file_id（只 URL/data URI） | 512MB/文件，2.5TB/项目；默认永久，可选 `expires_after` 1h–30d | 单图约 20MB；无需先上传（URL 可直传） |
| Anthropic | Files API（beta，头 `files-api-2025-04-14`） | Messages image source 支持 `base64` / `url` / `file(file_id)` | 500MB/文件，100GB/组织；保留到手动删除 | 文件操作免费，内容按输入 token 计费 |
| Google Gemini | Files API → `file.uri` | `file_data{file_uri,mime_type}` | 2GB/文件，20GB/项目，**48h 自动删除** | 存储免费；inline >20MB 应走 Files API |
| 阿里百炼/DashScope | 无图片 file_id | **公网 URL / base64 data URI / SDK 本地路径** | 单图 20MB（部分 10MB）；请求体 64MB（DashScope 域名 16MiB）；base64 最多 250 图 | file ID 仅 Qwen-Long 文档；无 TTL |
| OpenRouter | beta Files API（`or_file_`） | OpenAI 兼容 `image_url`（URL 或 data URI） | 100MiB/文件，10GiB/工作区，不过期 | Responses schema 已有 `file_id`，但图像指南只文档化 URL/base64，暂不宜依赖 |
| Bedrock | — | Converse `ImageSource` 原生 `bytes` 或 `s3Location` | — | pi 仅用 bytes |

## 2. 结论与设计

**要做，但定位为「平台 blob 协议 + 每厂商 adapter」，不是全链路统一 `file_id`。**

1. 客户端只上传平台一次：`POST /v1/files` → `file_id` + URL。
2. 对话消息只存 `{type:"image", blobId:"..."}`（本仓 `agentcore.ImageContent` 增加 `BlobID`，
   或新增 `ImageRefContent`），**不内联 base64**。
3. 出站时按 `(provider, model)` 能力选形态：`file_id` > `url` > `base64`，base64 永远兜底。
4. **关键认知**：base64-only 厂商（DashScope、部分网关）**每轮仍必须重传字节**；平台缓存只能省
   「客户端重复上传/重复编码」，不能省 wire 重传。需配合缩放压缩、prompt caching、
   历史图片省略/compaction。
5. URL 路径：平台发短 TTL 签名 URL；DashScope 要求 URL 响应含 `Content-Length`/`Content-Type`，
   且不能是内网地址。
6. provider file 缓存键 `(blob_hash, provider, account/workspace)`；Gemini 48h 过期需懒刷新；
   引用计数归零后 GC。

## 3. 本仓实现顺序

1. **Gemini Files API**（48h、2GB、最简单）→ **OpenAI Responses `file_id`** →
   **Anthropic URL（免上传）** → **Anthropic Files `file_id`** → **DashScope/OpenRouter URL**。
2. `internal/blob` 已有：上传（流式 spool + sha256 去重）、PG 64KiB 分块、CAS、base64 流式读出。
3. `internal/provider` 增加能力位 `SupportsImageURL` / `SupportsFileID`，以及
   `ResolveImage(blobID) (wireForm, error)`。
4. 迁移：pigo 的 `ImageContent{Data,MimeType}` 保留为 wire 形态；持久层只存 `BlobID`。

## 4. Go 类型草案

```go
type BlobRef struct {
    ID       uuid.UUID
    SHA256   string
    Size     int64
    MIME     string
    Width    int
    Height   int
    Storage  int16 // 1=pg_chunked 2=local_cas
}

type ProviderRef struct {
    FileID    string
    URI       string
    ExpiresAt time.Time
}

type ProviderFiles struct {
    Provider string
    Account  string
    Refs     map[uuid.UUID]ProviderRef // blob -> provider handle
}
```
