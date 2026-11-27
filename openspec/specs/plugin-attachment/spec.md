# plugin-attachment Specification

## Purpose
The attachment plugin surface describes how a message attachment is projected as metadata
(fileId/name/mime/size/kind/thumbnail/download/delivery), how a plugin claims a MIME range for
`keel.ui.message-part`, and how the host falls back to its default attachment row. Bytes stay
behind `file.read`; the projection never carries bytes, CAS paths, or over-permission URLs.

## Requirements

### Requirement: 附件只有一条生命周期

系统 SHALL 只通过 `POST /v1/files` 上传、以 `blobId` 引用、经
`user.message.attachments[]` 进入会话；插件 MUST NOT 获得第二套上传路由、blob 表或
附件引用表。事件日志 MUST 只持久化 `blobId` 与展示元数据，MUST NOT 写入字节、base64 或
CAS 路径。附件解析 MUST 复用现有 blob 引用计数、TTL 与模型派发路径。

#### Scenario: 上传后原生投递

- **WHEN** 客户端上传一个 PNG 并把它作为附件提交，且当前模型声明支持图片输入
- **THEN** 事件日志只出现 `blobId` 与 mime/size，模型请求收到原生图片部件

#### Scenario: 不支持时文本兜底

- **WHEN** 当前模型或协议不声明视频输入
- **THEN** 视频附件以包含 `blobId`、name、mime、size 与原因的文本块进入请求，不静默丢弃

#### Scenario: 无第二条链路

- **WHEN** 审查仓库的上传路由与附件持久化
- **THEN** 只有 `/v1/files` 一条上传路由、只有一组 blob 表/引用表，插件包不声明附件存储

### Requirement: 输入期附件校验

`POST /v1/sessions/{id}/input`（以及复用同一提交路径的 MCP Apps `ui/message` 与 retry）
MUST 在提交 `user.message` 前校验附件数组；任何失败 MUST 返回字段级错误且 MUST NOT 写事件。
每个输入 MUST 最多 16 个附件；单个附件 MUST 不超过部署的 `MaxUpload`（默认 64 MiB）与
租户存储配额；`fileId` MUST 属于当前 tenant 且 blob `complete=true`；声明的 MIME（小写、
去参数后）MUST 与 blob 记录的权威 MIME 一致。名称 MUST 限制在 1..256 字节且只作展示，
MUST NOT 产生路径语义。

#### Scenario: 超过条数

- **WHEN** 一个输入携带 17 个附件
- **THEN** 返回字段级 400，事件日志没有新的 `user.message`

#### Scenario: 跨租户或未完成

- **WHEN** 附件 `fileId` 属于另一租户，或对应 blob 仍在上传中
- **THEN** 返回字段级 400，且不区分“不存在”与“属于他人”

#### Scenario: MIME 伪造

- **WHEN** 客户端把一个 `text/plain` 的 blob 声明为 `image/png`
- **THEN** 返回字段级 400，不进入图片原生派发

#### Scenario: 超限文件

- **WHEN** 上传字节超过 `MaxUpload`
- **THEN** 返回 413，blob 不进入完成态

### Requirement: 插件只看到附件元数据

宿主 SHALL 向插件 UI 发布只读附件投影，至少包含 `fileId/name/mime/size/kind`、可选
`width/height`、`thumbStatus`、同源 `thumbnailUrl/downloadUrl` 与本次投递的
`delivery.native/reason`。投影 MUST NOT 包含字节、base64、data URI、CAS 路径、租户头、
cookie、凭据或跨租户 URL。URL MUST 是同源、受当前 principal 与租户鉴权的普通 blob 路由，
MUST NOT 是跨租户或永久签名 URL。系统 MUST NOT 向插件后端提供附件字节读取句柄。

#### Scenario: 预览拿到元数据

- **WHEN** 一个附件渲染贡献被宿主调用
- **THEN** 它收到元数据与同源 URL，收不到任何字节或 base64

#### Scenario: 跨租户不可读

- **WHEN** 租户 B 的渲染器用租户 A 的 `fileId` 请求缩略图或下载
- **THEN** 返回 404，且不泄露该 blob 是否存在

#### Scenario: 名称只展示

- **WHEN** 附件名为 `../../etc/passwd` 或包含控制字符
- **THEN** UI 仅按文本展示/转义，不产生文件路径或链接目标

### Requirement: 附件预览与渲染槽位

附件预览 MUST 复用 `keel.ui.message-part` 槽位；插件 MUST 以 `part=attachment` 声明认领，
并 MAY 在 `extensibility.json` 用 `mime` 模式收窄匹配；未提供 `extensibility.json` 的包
MUST NOT 被识别为附件渲染器。宿主 MUST 按声明顺序选择第一个
ready、target 可用且 MIME 匹配的贡献；没有匹配、贡献不可用或渲染抛错时 MUST 回退到宿主
默认附件行。渲染器 MUST 保留文件名、大小与下载/打开入口，MUST NOT 隐藏这些事实或把附件
变成不可下载的纯视觉块。`iframe` 贡献 MUST 显式标注“非原生主题”；TUI 对 `iframe` MUST
显示不可用与图形界面打开入口，`chunk` MUST 按 TUI 视图渲染。

#### Scenario: PDF 预览命中

- **WHEN** 一个 ready 的贡献声明 `part=attachment` 且 `mime` 匹配 `application/pdf`
- **THEN** PDF 附件显示该预览，同时保留文件名、大小与下载入口

#### Scenario: 渲染失败回退

- **WHEN** 附件预览贡献抛错或加载失败
- **THEN** 该附件显示默认附件行与可解释的失败态，其他附件和会话不受影响

#### Scenario: 无匹配

- **WHEN** 没有任何贡献匹配该 MIME
- **THEN** 显示宿主默认附件行，不出现空白或半成品入口

#### Scenario: TUI 降级

- **WHEN** 同一附件在 TUI 中渲染，且命中的是 `iframe` 贡献
- **THEN** 显示“终端不可用（需要图形界面）”与在浏览器打开的入口，不渲染 iframe

### Requirement: 附件预览可逆且租户隔离

插件停用或卸载 MUST 在同一 generation 撤销其附件预览贡献与订阅，MUST NOT 留下缓存、
监听器或半渲染行；默认附件行 MUST 继续可用。安装审批 MUST 展示该插件将认领的槽位、
`part=attachment` 与 MIME 列表。附件投影 MUST 按 tenant 与 session 归属过滤，blob 读取
MUST 带 tenant 谓词；跨租户附件 MUST 不可见、不可读。插件工具若被预览调用，MUST 继续受
`plugin.invoke` 与 `permissions.tools` 约束，拒绝 MUST 写审计。

#### Scenario: 停用即撤销

- **WHEN** 停用一个提供 PDF 预览的插件
- **THEN** 已有与新的 PDF 附件都回到默认附件行，无残留预览或订阅

#### Scenario: 安装审批可见认领

- **WHEN** 审批一个声明附件预览的插件安装
- **THEN** 审批内容包含槽位、form、`part=attachment` 与 MIME 列表

#### Scenario: 越权工具调用

- **WHEN** 附件预览经桥调用一个未在 `permissions.tools` 声明的工具
- **THEN** 返回类型化拒绝并写 `plugin_audit`，预览显示无权限态

### Requirement: 失败原因、五态与审计

附件预览与附件行 MUST 覆盖加载、空、错误、无权限、成功五态。附件解析降级 MUST 写结构化
日志，包含 `fileId/size/mime/reason`，MUST NOT 记录文件名、正文或字节。插件生命周期与
越权拒绝 MUST 写审计；审计 MUST NOT 含附件内容、文件名、base64 或完整 URL 凭据。

#### Scenario: 降级可追踪

- **WHEN** 图片因 blob 缺失走文本兜底
- **THEN** 日志给出 `blobId`、mime、size 与失败原因，模型收到带同一原因的文本块

#### Scenario: 无权限态

- **WHEN** 当前账号没有读取该会话/附件所需的权限
- **THEN** 预览显示无权限态，后端返回 403/404，不流式返回任何 blob 字节

#### Scenario: 审计不含内容

- **WHEN** 安装附件预览插件并触发一次越权拒绝
- **THEN** 审计只含 pluginId、fileId 或哈希、mime、结果与原因，不含文件名或正文

### Requirement: 不引入第二套附件框架

附件能力 MUST 复用现有 `internal/blob`、`internal/httpapi` 文件路由、`internal/agent`
的派发与 `keel.ui.message-part` 槽位机制；MUST NOT 新增附件专属 upload API、附件表、
预览缓存服务或第二套模型输入转换。媒体生成工具 MUST 保持“产物生成器”定位，MUST NOT 被
改造成附件输入通道。

#### Scenario: 预览不写 blob

- **WHEN** 一个插件渲染附件预览
- **THEN** 它不创建、修改或删除任何 blob，也不新增引用计数

#### Scenario: 媒体工具边界

- **WHEN** 模型调用 `image_generate`
- **THEN** 它生成产物并按既有规则引用；不改变附件输入/派发语义
