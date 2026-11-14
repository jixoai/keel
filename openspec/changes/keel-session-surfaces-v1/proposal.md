# Proposal: keel-session-surfaces-v1

## Why

一个会话不该永远是一套「文本输入 + 对话历史」的壳。Provider 的种类决定用户如何输入、如何看到
结果：

- **LLM**：文本输入；对话历史。
- **realtime**：持续双向语音；转写历史 + 音频播放状态（真实音频回放待资产接线）。
- **image**：prompt/参考图输入；生成、编辑、多轮编辑的画廊。
- **TTS/STT**：文本/音频工作台；转写或合成历史。

当前客户端只有一种 Chat 壳，且 Provider 类型里缺了 **realtime**。这会导致：选 realtime 模型
仍然显示文本框；选 image 模型仍然显示聊天消息；AgentTeam 队长是 realtime 时也没有对应主面板。

本 change 把「会话面板」从聊天壳里解耦成 **provider-kind surface registry**，并补齐 realtime
Provider 与实时语音会话。

## What Changes

### 1. Provider-kind surface

- 每个 Provider 类型声明一个 **surface descriptor**：`surfaceKind`、composer 形态、timeline
  形态、能力（附件/音频/参考图/多轮编辑）、状态与事件映射。
- 客户端按会话解析出的 Provider 类型装载对应 surface；不再在 `Conversation` 里硬编码
  「消息列表 + 文本框」。
- 内置 surface：`llm` / `realtime` / `image` / `tts` / `stt`；`video` 与未知类型使用专用占位面
  （类型名 + 原因 + 「打开模型设置」，不渲染输入框、不伪造结果）；插件可声明自定义 surface 或
  覆盖默认 composer/timeline 贡献。未知 provider type MUST NOT 被当作 llm 聊天壳。
- 会话的 `surfaceKind` 在会话打开/选型时由所选模型/队长默认模型的 descriptor 解析；同一 surface
  内可换模型；**跨 surface 换模型必须新建会话**（历史结构不兼容），UI 给出明确提示。

### 2. realtime Provider

- 新增 Provider 类型 `realtime`，能力包括：音频输入/输出、VAD、打断、增量转写、工具调用、
  文本兜底。
- 实时通道：客户端麦克风 → 会话 realtime endpoint（本轮为同源 WebSocket `/v1/realtime`；
  WebRTC 与插件 descriptor 选择 transport 尚未实现）；
  Provider 返回音频帧 + transcript delta/final + tool calls。
- 客户端 MUST 先 GET `/v1/realtime/status` 预检；`configured:false` 时显示配置态，不建立 WebSocket。
- 会话账本目标：只持久化**最终 transcript + 音频资产引用**；不把每个音频 chunk 写成事件。
  本轮交付只做客户端投影，不写服务端账本（刷新不恢复，见 tasks 暂缓项 (a)）。
- 支持打断（barge-in）、静音/停止、重连（重连后明确是新的 Provider 会话，已 final 的转写不重放）、
  权限指示、无麦克风降级为文本。
- 标准模式下选 realtime 模型即进入实时对话；AgentTeam 模式下队长默认模型是 realtime 时，
  主会话进入实时对话，成员仍可按各自 surface 运行。

### 3. Image surface

- 输入区围绕：prompt、negative prompt、参考图、宽高比/分辨率/seed 等 Provider schema 参数。
  **本轮**参考图只在演示路径完整生效；真实生成请求只上报 prompt/negative/size/n。
- 会话区是画廊：生成结果、编辑历史、多轮编辑（选图 + 指令 → 新版本）、复用为参考图；
  下载/复制资产引用/删除未提供（等待资产引用与字节落盘语义）。
- **本轮**不把产物字节写入 layerfs：真实结果以 Provider 返回的 http(s) URL 进入画廊，画廊只持有
  URL 与元数据、不内联 base64；多轮编辑保留 `parentAssetId` 版本链；真实编辑只接受 http(s) URL。

### 4. 实时编译与运行

- 每次可用增量后重新构建 `web/` 并重启 `keel serve`，通过 `local-gaubee-keel-app.fde2e.com`
  暴露给 Owner；交付链接不使用 localhost。
- 运行实例使用本地 Postgres 与 fake/realtime demo provider；真实 Provider 不可用时明确标注
  demo，不伪造真实模型结果。

## Non-Goals

- 不在本 change 实现完整的 WebRTC 媒体服务器；本轮只交付内置 WebSocket Provider
  （`/v1/realtime`），插件 descriptor 选择 transport 为后续项。
- 不把音频字节写进事件账本；只存资产引用。
- 不在本 change 做语音克隆/声纹训练。
- 不让 surface 逻辑进入内核；surface 是 Harness/UI 层。
- 不要求跨 surface 复用同一会话历史；跨 surface 明确要求新会话。

## Impact

- **Provider 类型**：新增 `realtime`；`/v1/models` 投影 `providerType`/`surfaceKind`，surface
  descriptor 由模型/Provider 声明推导。
- **数据库**：本 change **未新增** session `surface_kind`/`surface_config` 列；会话选型随输入携带，
  surface 由客户端按模型 descriptor 解析。realtime transcript/audio asset refs 的服务端账本
  写入为暂缓项（tasks (a)）。
- **API**：`GET /v1/realtime`（WebSocket 桥）、`GET /v1/realtime/status`（预检）；surface 事件映射；
  媒体能力 `POST /v1/media/images/generate|edit`、`POST /v1/media/decision`、
  `POST /v1/decisions/ask`；`/v1/media/capabilities`。音频资产上传/读取未实现。
- **客户端**：`SessionSurface` registry（含 `resolveCaptainSurface` / `previewSwitchReason`）；
  LLM/Image/Realtime/Speech 面板 + video/generic 占位面；AgentTeam captain surface 判定接缝；
  移动端语音权限与降级。
- **插件**：Provider 类型声明 surface descriptor；自定义 surface 走 UI contribution。
- **测试**：surface 选择、跨 surface 换模型（含 demo preview 门）、realtime 预检/转写/打断/重连、
  image 多轮编辑、媒体编辑 URL-only、移动端与可访问性。
