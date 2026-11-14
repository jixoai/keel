# Design: keel-session-surfaces-v1

## 1. 核心模型：Provider kind 决定 Session Surface

```
ModelDescriptor.providerType
        │
        ▼
Session.surfaceKind  ──► SessionSurface
                              ├─ Composer
                              ├─ Timeline
                              ├─ Capability strip
                              └─ Event projector
```

- `surfaceKind` 由会话启动时解析出的 Provider 类型决定：
  - `llm` → `llm` surface
  - `realtime` → `realtime` surface
  - `image` → `image` surface
  - `tts` / `stt` → `speech` surface（mode 区分）
  - `video` → 占位面（`video` surface 尚无专用面板，见 §8）
  - 未识别 provider type → `generic` 占位面（reason 点名该类型，见 §8）
- Provider 类型可以声明 `surface` descriptor；客户端内置同名 surface 优先使用。没有内置面板的
  类型（`video` 与未知类型 `generic`）不回落文本聊天，而是装载 `surfaces/unsupported/` 占位面：
  显示类型名、原因与唯一的「打开模型设置」；占位面不渲染 composer、不生成任何结果（见 §8）。
- **跨 surface 换模型**：允许在同一 surface 内换模型；从 `llm` 换到 `realtime`/`image`
  必须新建会话。UI 在选择器里给出“将新建会话”提示，不得把旧历史伪装成新 surface 的历史。
  演示预览（`store.surfacePreview`）同样受此门禁约束：preview 不是会话，只能接管空面板，
  不得把已开会话的其它 surface 顶替成演示面板（`previewSwitchReason`）。
- **队长模型决定主 surface**：AgentTeam 的 captain 默认模型经 `resolveCaptainSurface` 解析；
  没有 captain 或 captain 模型无法解析时回退 `llm`，不猜测 generic/realtime（见 §6）。

## 2. Surface 契约

```ts
interface SurfaceDescriptor {
  kind: "llm" | "realtime" | "image" | "video" | "speech" | "generic";
  label: string;                       // picker / surface header text
  composer: "text" | "voice" | "prompt-media" | "audio-file";
  timeline: "messages" | "transcript" | "gallery" | "transcript-audio" | "audio-history";
  speechMode?: "tts" | "stt";          // the speech surface's workbench
  capabilities: {
    audioInput?: boolean;
    audioOutput?: boolean;
    interruption?: boolean;
    referenceImages?: boolean;
    multiTurnEdit?: boolean;
    toolCalls?: boolean;
    streamingTranscript?: boolean;
    gallery?: boolean;
  };
  reason?: string;                     // why a medium has no panel (video/generic)
  paramsSchemaRef?: string;
}
```

- `video` / `generic` 的 descriptor 仍可声明 composer/timeline 作为能力事实，但 shell 不装载它们，
  只装载占位面（§8）——descriptor 是对 Provider 能力的声明，不是对已交付面板的承诺。

- Surface 只消费 Harness 事件与 Provider 能力，不直连内核。
- Composer 与 Timeline 是两个可独立替换的组件；同一 surface 可以组合不同 composer/timeline。
- Surface 必须覆盖五态：空/加载/错误/权限不足/成功；实时语音还必须覆盖麦克风未授权、
  无设备、连接中、重连、被打断、静音。
- Surface 组件来自 shadcn/NSH 官方件；不手写仿制。

## 3. LLM surface（基线）

- Composer：文本、附件、queue/guide、停止、模型切换（同 surface）。
- Timeline：用户/助手/thinking/tool group/approval/compaction；保持现有行为。
- 验收：现有 web 行为不回归。

## 4. Image surface

### 4.1 Composer

- prompt（多行）+ negative prompt（Provider 支持时）
- 参考图：选择（系统图片选择器）；缩略图 + 删除 + 作为参考图复用。拖拽/粘贴尚未实现。
  **本轮**参考图只在演示路径完整生效；真实生成请求只上报 prompt/negative/size/n，
  真实参考图上传/引用仍是缺口。
- Provider schema 参数：aspect ratio、resolution、seed、steps、guidance、reference count 等，
  只渲染模型声明支持的字段；当前网关声明的是 `size` / `n`（UI 的 aspect/quality/count/seed
  映射到这两个字段及本地生成参数）
- 主操作：生成；次操作：清空/复用上次参数
- 忙碌时：显示排队/生成中/取消；不允许多个生成争抢同一 slot（按 Provider 并发能力）

### 4.2 Timeline

- 画廊按 turn 分组：prompt → 生成资产（缩略图/大图预览）→ 参数摘要 → 操作
- 操作（本构建）：预览、作为参考图（演示路径）、编辑、重新生成。下载 / 复制资产引用 / 删除
  未提供——当前资产是演示本地图或 Provider URL，尚无资产引用与字节落盘语义（tasks S2）。
- 多轮编辑：选一张资产 + 编辑指令 → 新版本；保留 `parentAssetId`，可回溯版本链。
  版本链是客户端投影；真实编辑仅接受父资产的 Provider http(s) URL，演示资产不能作真实编辑输入。
- 资产引用：**本轮不写 layerfs**，真实结果以 Provider 返回的 URL 进入画廊，画廊不内联字节；
  加载失败显示占位与重试。字节落盘/上传为后续项。
- 空态：示例 prompt + “先配置 image Provider” CTA；未配置时进入明确标注的演示模式，不冒充真实输出。

### 4.3 状态

- idle / generating / succeeded / failed / cancelled；配额/权限态待真实 Provider 接线后补齐
- 生成失败保留 prompt 与参数，可重试；不丢用户输入；真实网关 503 标记配置态

## 5. Realtime surface

### 5.1 交互

- **持续双向会话**：用户点「开始实时对话」建立长连接，麦克风音频持续上行、模型音频持续下行并边收边播；
  这不是“录音 → 转写 → 显示文本”的 STT 流程。后者属于 `speech` surface 的 STT 模式。
- 主输入：常驻麦克风流（VAD 断句由 Provider/会话配置决定）；显式麦克风权限与通话状态
- 主时间线：**流式转写**（user/assistant transcript delta/final）+ assistant 音频播放状态；音频不是时间线的主输入
- 状态：连接中 / 就绪 / 聆听 / 思考 / 说话 / 被打断 / 重连 / 静音 / 未配置 Provider
- 打断：VAD 检测用户开口或点“打断”即取消当前 response、立即停止播放并标记当前 assistant 行为 interrupted
- 兜底：麦克风不可用/拒绝授权 → 同一 surface 内切换文本 `response.create`，不丢会话；无 Provider 时显示配置态

### 5.2 传输与 Provider

- 目标形态：Provider 类型 `realtime` 声明 transport：`websocket` / `webrtc`；客户端按 descriptor 连接。
  **本轮交付**使用内置 WebSocket Provider（`KEEL_REALTIME_*` 环境配置）；`webrtc` 与
  “插件 descriptor 选择 transport”尚未实现，hot path 固定为同源 `/v1/realtime` WebSocket。
- 客户端 → 服务端：音频帧（PCM/Opus）、VAD 事件、打断、文本兜底输入
- 服务端 → 客户端：音频帧、`transcript.delta`、`transcript.final`、`tool.call`、`error`
- 内置 Provider 负责厂商协议；Harness 负责会话、资产、工具、账本与权限
- **浏览器音频宿主**：`web/host.js` 负责 getUserMedia、PCM16 采集/重采样、AudioContext 播放、
  WebSocket 生命周期与 barge-in；NSH app 只通过 `keel.realtime.*` 调用与事件驱动 UI，
  不直接碰音频设备。
- **服务端桥**：`/v1/realtime` 用服务端凭证连接 Provider（Qwen-Omni/OpenAI 等），
  双向透传 JSON 事件；密钥只在服务端环境，浏览器拿不到 Provider key。
  客户端 MUST 先 GET `/v1/realtime/status` 预检；`configured:false` 时落配置态，不建立 WS。
- 会话账本目标：只写最终 transcript 与音频资产引用；音频 chunk 不落账本。
  **本轮交付不写服务端账本**：final transcript 只进客户端投影，刷新不恢复（tasks 暂缓项 (a)）。
- 重连：客户端重连后明确标注“新的 Provider 会话，断线前的转写不会重放”；本地已 final 的
  transcript 不重复，partial 可丢弃/替换。没有 Provider 会话恢复能力时不得声称“已按游标续接”。

### 5.3 事件映射

| 实时事件 | 目标账本/投影（本轮只投影，不写服务端账本） |
|---|---|
| user transcript final | `user.message`（text + audio asset ref） |
| assistant transcript final | `assistant.message`（text + audio asset ref） |
| assistant partial | 仅 UI partial，不写账本 |
| tool call | 既有 tool.result / 工具事件 |
| interrupt | UI 标记 partial；终态消息带 `interrupted:true` |
| connection error | surface 状态，不伪造 assistant 消息 |

### 5.4 安全

- 麦克风必须显式授权；录制中持续可见指示；停止即释放
- 不后台常听；不把原始音频写日志；会话与媒体请求按租户隔离（音频资产隔离在资产存储落地后成立）
- 打断/停止必须取消 Provider 音频与生成，不留后台播放

## 6. AgentTeam 集成

- Team 的 captain 默认模型决定主会话 `surfaceKind`。
- 标准模式：选 realtime 模型 → realtime surface；选 image → image surface。
- AgentTeam：captain realtime → 主会话实时语音；成员按各自 Agent 默认模型运行，团队抽屉显示
  成员状态与 surface 标记。
- 成员 surface 不与 captain 混用：captain 的 timeline 只显示 captain 的转写/工具；成员结果通过
  团队消息/任务卡进入 captain 时间线。
- 跨 surface 换 captain 模型：要求新会话。
- **交付现状**：`resolveCaptainSurface` 已实现并由 `registry.test.ts` pin（captain 模型 → surface；
  无 captain / 无法解析 → `llm`）。web 团队界面、成员卡 surface 标记与“成员结果进 captain 时间线”
  尚未实现，依赖 `keel-agent-team-v1`（tasks 暂缓项 (c)）。

## 7. 客户端结构

```
web/src/surfaces/
  registry.ts            resolveSurface / resolveSelectionSurface / resolveCaptainSurface
                         / checkSurfaceSwitch / previewSwitchReason / surfaceFact
  image/index.tsx        图像工作台（演示画廊 + 真实网关 generate/edit）
  image/harness.tsx      独立 TUI 夹具
  realtime/index.tsx     实时语音投影（麦克风/桥由 web/host.js 负责）
  realtime/components/   通话控件、转写行、状态条、波形、音频状态
  speech/index.tsx       stt/tts 工作台（两种 mode）
  speech/components/     录音按钮、转写行、波形
  unsupported/index.tsx  video/generic 占位面（label + reason + 设置 CTA）
```

- `ConversationScreen` 只负责解析 `surfaceKind` 并渲染对应 surface；分派对 `llm / realtime /
  image / speech / video / generic` 穷尽，`video`/`generic` 装载占位面，不回落聊天壳。
- `session.store` 保留 `surfaceKind` 的选型来源与 `surfacePreview`；模型选择时用
  `checkSurfaceSwitch` / `previewSwitchReason` 校验是否允许跨 surface。
- 现有 `messages.tsx`/`composer.tsx` 保留为 llm surface 实现，不复制一套聊天壳。

## 8. 未提供专用面板的类型（video/generic）

- `video` 是已知 medium，但当前没有已接入的视频 Provider，也没有专用面板；未识别的 provider
  type 解析为 `generic`，reason 点名该类型。两者都装载 `surfaces/unsupported/` 占位面。
- 占位面内容：surface label、原因（`descriptor.reason` 优先；`video` 有自己的句子）、唯一的
  主动作「打开模型设置」；只有演示预览态才出现次动作「返回对话」（离开预览是一次真实导航）。
- 占位面不渲染 composer、不显示空对话历史、不播放或生成任何结果；文案必须说明“当前没有已接入的
  Provider”，不得用演示结果冒充真实结果。
- 模型选择器的事实行同样诚实：`video` / `generic` 显示「暂无专用面板」，不承诺会打开一个不存在
  的面板。
- `Conversation` 的分派是穷尽式的：`video` / `generic` 分支显式存在，新增 surface kind 时
  穷尽检查会暴露未处理项；禁止用 `else` 沉默回落到 llm。
- 这是“不伪造结果”原则在面板层的落地：没有 Provider 的 medium 只给配置路径，不给假输入框、
  假结果或假会话。决策类 provider（`decision` family）目前也解析为 `generic` 占位面，直到有
  专用面板。

## 9. 实时开发与验收环境

- 每次可运行增量：`nsh package --target web` → 重启 `keel serve`（13760）→
  `pnpm tunnel:expose keel-app --port 13760 --pin`。
- 交付链接：`https://local-gaubee-keel-app.fde2e.com`，密码单独给 Owner。
- fake/realtime demo Provider 可用时明确标 demo；真实 Provider 未配置时显示配置 CTA，不伪造结果。

## 10. 验收标准（按任务）

1. **Surface registry**：选择 llm/realtime/image/speech 模型分别进入对应 surface；跨 surface 换模型
   被阻止并提示新建会话（演示预览同门禁）；`video`/未知类型装载占位面并显示原因，不回落文本聊天。
2. **LLM surface**：现有会话/消息/工具/审批/queue-guide 行为不回归；`nsh test --target tui`
   通过。
3. **Image surface**：生成 → 画廊资产；参考图选择/复用（演示路径）；选图编辑 → 新版本；参数只显示
   schema 字段；失败保留输入；空/错误/权限态可见；真实编辑只接受 http(s) URL。
4. **Realtime surface**：麦克风授权/拒绝；预检 `configured:false` 时落配置态且不开 WS；
   聆听→转写→assistant 回复；打断停止播放；重连不重复 final transcript，且明确标注新 Provider
   会话；无麦克风降级文本。账本持久化是暂缓项（tasks (a)），本轮验收只到客户端投影不重复。
5. **AgentTeam**：captain 判定接缝（`resolveCaptainSurface`）单测通过；无 captain 回退 llm；
   web 团队界面不在本 change（tasks 暂缓项 (c)）。
6. **移动端**：390px 单列；麦克风权限/录音指示可见；image 画廊可滚动；无横向溢出。
7. **运行**：web 构建通过，`keel serve` 启动，local 域名可访问；登录后能看到对应 surface。
8. **安全**：音频/麦克风无后台常听；无原始音频日志；媒体请求走租户边界；错误不伪造消息。

> 本轮修订（2026-11-13）：按已交付代码事实同步——unsupported 占位面（video/generic）、
> `resolveCaptainSurface` / `previewSwitchReason`、realtime 预检未配置态、媒体编辑 URL-only，
> 并显式登记三项暂缓（转写账本、真实厂商凭据、AgentTeam web 团队界面）。
