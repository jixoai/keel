# Proposal: keel-plugin-attachment

> 立项：2026-11-26。基线：`docs/MODEL-INPUT-MODALITIES.md`、`docs/BLOB-LIFECYCLE.md`、
> `internal/agent/{attachments,images,messages}.go`、`internal/blob`、`POST /v1/files`。
> 参考：DSH `deepseek-ai/dsh-superagent-prototype@8c32a9dc`（`src/app/assessment.ts`、
> `src/sources/feishu-image.ts`）把图片当作**有界引用**显式读取，与本 change 的
> “元数据进插件、字节留在宿主”取向一致；不引入其 Ticket 模型。
> 定位：附件是**一条**从 blob 到模型输入/插件渲染的生命周期，不是第二套文件系统，
> 也不是插件自己的上传通道。

## Why

附件主链路已经存在：`POST /v1/files` 落 blob，`user.message.attachments[]` 存
`blobId` 引用，`internal/agent/attachments.go` 按模型声明与协议能力决定原生部件或
文本兜底，`internal/blob` 负责引用计数、TTL 与缩略图。但插件生态缺少契约：

- 插件没有“附件是什么”的只读投影；想画预览只能自己猜字段或绕过宿主直接读 blob，
  容易长出第二套上传/存储；
- `POST /v1/sessions/{id}/input` 对 `attachments` **原样透传**：没有每输入条数、大小、
  MIME 一致性、完成态与租户归属校验；声明 MIME 与 blob MIME 不一致时可以骗过模态判断；
- `keel.ui.message-part` 已有 `part` 匹配，但没有附件渲染器的接入口径、MIME 收窄、
  失败回退与“不得隐藏原始文件名/下载”的约束；
- 失败路径（provider 拒绝、blob 缺失、缩略图 failed、上传未完成）散落在日志里，
  插件与用户看到的降级原因不一致。

本 change 冻结附件的唯一生命周期与插件面投影，避免下一波插件重复造上传、字节读取或
预览缓存。

## What Changes

1. **唯一生命周期**：上传（`POST /v1/files`）→ 客户端引用（`fileId`）→
   `user.message.attachments[]` → durable ref（`ImageRefContent`/`MediaRefContent`）→
   模型原生部件或文本兜底 → 引用计数/TTL 回收。MUST NOT 新增第二套上传路由、blob 表或
   附件框架。
2. **输入期校验**：每个输入最多 16 个附件；单文件默认上限沿用 `MaxUpload`（64 MiB）；
   `fileId` MUST 属于当前租户且 `complete=true`；声明 MIME MUST 与 blob 记录一致（归一后
   比较）；文件名只作展示，不产生路径语义；超限/非法 MUST 返回字段级 400/413，不落事件。
3. **插件只读投影**：宿主向插件 UI 提供附件元数据（`fileId`/name/mime/size/kind/尺寸/
   缩略图状态/同源 URL/本次投递形态），MUST NOT 提供字节、base64、CAS 路径或凭据。
4. **预览/渲染槽位**：复用 `keel.ui.message-part`；贡献以 `part=attachment` 认领，
   `extensibility.json` 可用 `mime` 收窄；宿主按声明顺序选择第一个 ready 且匹配的贡献；
   没有匹配或渲染失败时保留宿主默认附件行。渲染器 MUST NOT 隐藏文件名、大小与下载/打开入口。
5. **租户与权限**：附件读取始终带租户谓词；跨租户 `fileId` 与不存在不可区分；插件安装
   审批展示它将认领的附件槽位与 MIME；插件工具若被附件预览调用，仍走 `plugin.invoke` 与
   `permissions.tools`，拒绝写审计。
6. **失败与清理**：provider 不支持/解析失败/上传未完成/缩略图失败都有稳定原因；
   插件停用/卸载 MUST 在同一 generation 撤销预览贡献；MUST NOT 留下缓存、订阅或孤儿文件。
7. **可观测与五态**：预览区覆盖加载/空/错误/无权限/成功；附件解析降级写结构化日志
   （fileId/size/mime/原因，不含文件名、正文或字节）；审计不记录附件内容。

## Capabilities

### New Capabilities

- `plugin-attachment`: 附件的唯一生命周期、输入校验、插件只读投影、预览槽位契约与
  租户/失败/审计边界。

### Modified Capabilities

<!-- 复用 plugin-system / plugin-ui-extension / blob-lifecycle / provider-files 的既有
     requirement；本 change 只新增附件 capability。若实现必须改既有 requirement 的
     正文，实现阶段补精确 delta，本提案不虚构修改。 -->

## Impact

- `internal/pluginpkg`（附件贡献的 `part`/`mime` 声明与校验）、`internal/harnessext`
  （extension 投影、插件视图）、`internal/httpapi`（输入期附件校验）、
  `internal/agent`（复用现有解析，不重写）、`internal/blob`（只读引用）、
  `web/src/screens/messages.tsx` 与 `web/src/plugins/*`（预览槽位与五态）。
- 不新增表、不新增上传路由；不改事件日志的 `blobId` 持久形态；不引入外部依赖。
- 测试：输入校验（数量/大小/MIME/完成态/租户）、provider 纳管/兜底、插件预览选择与回退、
  停用零残留、TUI 降级、真实浏览器 E2E。

## Out of scope

- 插件后端读取附件字节、服务端转码/OCR/PDF 抽页、视频抽帧。
- 签名 URL、跨租户共享、附件版本/编辑、附件市场。
- 把媒体生成工具（`mediaadapter`）改造成附件输入；它们仍是产物生成器。
- 第二套上传/存储/引用表或“插件自带附件目录”。
