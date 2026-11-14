# Realtime Provider 配置、协议与验收

> 证据口径：本文只描述当前工作树里**代码实际实现**的行为。每条结论都给出
> `路径:行号`，不把厂商文档、额度状态或历史探测结果当作代码契约。
>
> 密钥纪律：`KEEL_REALTIME_API_KEY` 只存在于服务端进程环境；浏览器、模型配置和
> 本文都不记录密钥。Provider 的 URL 和 key 不会下发给浏览器
> （`internal/httpapi/realtime.go:1-8`、`internal/realtime/provider.go:49-62`）。

## 0. 结论速览

- **realtime 是双向连续语音**：麦克风持续上行、模型音频持续下行、服务端 VAD
  断句、用户可随时打断。转写只是伴随事件，**不是输入机制**；它明确不是
  「录一段音，再送去转写」（`web/src/surfaces/realtime/index.tsx:1-25`、
  `web/src/surfaces/realtime/index.tsx:1088-1097`）。
- **未配置 `KEEL_REALTIME_WS_URL` 时（且 demo 关闭，`KEEL_DEMO_PROVIDER` 未开）**：
  `GET /v1/realtime` 返回
  `503 {"error":"realtime provider is not configured"}`；
  `GET /v1/realtime/status` 返回 `200` 且 `configured:false`，不是错误
  （`internal/realtime/provider.go:77-80`、
  `internal/httpapi/realtime.go:88-101`、
  `internal/httpapi/realtime_test.go:343-370`、
  `internal/httpapi/realtime_test.go:631-680`）。
  `KEEL_DEMO_PROVIDER=1` 时同一位置由 demo provider 接管（见 §2.2b）。
- **Provider 配置只来自 Keel 进程环境**（`KEEL_REALTIME_*`），不来自
  `internal/modelcfg` 的 Provider 配置、租户密钥或前端模型注册表
  （`internal/httpapi/realtime.go:67-70`、
  `internal/realtime/provider.go:68-96`）。
- **浏览器只连同源 `/v1/realtime`**，请求里只能出现 `session_id`、`model` 和
  被忽略的 `provider_id`；浏览器永远看不到 Provider URL 和 key，Provider 也收不到
  tenant/session id（`internal/httpapi/realtime.go:72-108`、
  `internal/realtime/provider.go:160-164`、`web/host.js:333-342`）。
- **UI 先 preflight 再开 WebSocket**：先 `GET /v1/realtime/status`；未配置直接进入
  配置态，不打开一个最终以 1006 结束的 WebSocket
  （`web/src/surfaces/realtime/index.tsx:717-762`、
  `web/host.js:898-978`）。

---

## 1. 概念对比：realtime 与 speech surface

### 1.1 一张表

| 维度 | realtime（实时语音） | speech · STT（语音转写） | speech · TTS（语音合成） |
| --- | --- | --- | --- |
| 会话形态 | 双向连续音频会话 | 录音 → 转写记录的工作台 | 文本 → 音频的配置表单 |
| 如何开始 | 按一次「开始实时对话」，之后持续通话 | 按一次录音，停止即释放麦克风 | 填文本，按「合成」（当前不可用） |
| 上行 | 麦克风 PCM16 持续上行 `input_audio_buffer.append` | 浏览器 `Speech` 会话，本地识别 | 无麦克风上行 |
| 下行 | Provider 音频 `response.audio.delta` 边收边播 | 只有文字转写，没有助手回复 | 当前不生成、不播放任何音频 |
| 断句 | Provider `server_vad`，`speech_started` / `speech_stopped` | 识别器的 interim / final 结果 | 无 |
| 打断 | 支持；开口或手动「打断」立即停止音频并取消生成 | 没有助手，不存在打断 | 无 |
| 转写的角色 | 伴随事件，用于 UI 投影；不是输入方式 | 产品输出本身 | 输入文本 |
| 面板能力 | `audioInput/audioOutput/interruption/streamingTranscript/toolCalls` | `audioInput/audioOutput` 共用 speech 面板 | `speech` 面板 descriptor 同时声明 audio 输入/输出；TTS 实际只有配置态 |
| 持久化 | 当前构建不写账本，刷新即丢 | 当前构建只在页面内保留 | 无结果 |

证据：

- realtime 面板能力：`web/src/surfaces/registry.ts:67-79`。
- speech 面板能力：`web/src/surfaces/registry.ts:95-101`。
- STT 使用浏览器 `Speech`，不是服务端 ASR：
  `web/src/surfaces/speech/index.tsx:11-29`、`web/src/surfaces/speech/index.tsx:262-319`。
- TTS 当前是配置态，合成按钮硬编码 `disabled`：
  `web/src/surfaces/speech/index.tsx:764-766`、
  `web/src/surfaces/speech/index.tsx:847-856`。
- 图片媒体网关没有音频能力，客户端明确把 video/tts 排除在外：
  `web/src/data/media.ts:1-17`、`web/src/data/media.ts:88-90`。
- 持久化：realtime 不写账本（`web/src/surfaces/realtime/index.tsx:20-25`）；
  speech 的转写行同样只是页面 signal（`web/src/surfaces/speech/index.tsx:185-192`）。
- 服务端没有 `/v1/audio/*` 路由；媒体路由只有图片生成/编辑、decision 和 LLM chat：
  `internal/httpapi/server.go:146-172`、`internal/httpapi/media.go:21-30`。

### 1.2 realtime 明确不是「录音后转写」

`RealtimeSurface` 的文件头把这条写成第一句：这不是 record-then-transcribe 面板；
按下「开始实时对话」会打开真实双向会话，浏览器保持 WebSocket，持续上传 16 kHz
PCM16，并边收边播 Provider 的 24 kHz PCM；转写是伴随音频的事件，不是输入机制
（`web/src/surfaces/realtime/index.tsx:1-8`）。空态文案也直接告诉用户：
「麦克风持续收音、助手语音边说边播，不是录音后再转写」
（`web/src/surfaces/realtime/index.tsx:1092`）。

因此：

- 用户停止说话后，由 Provider 的 VAD 结束这一轮，而不是用户按「停止录音」。
- 用户可以在助手说话时直接开口打断；UI 会立即停止播放并取消当前 response
  （`web/host.js:455-471`、`web/src/surfaces/realtime/index.tsx:864-874`）。
- 转写行是事件投影的结果；它不是上传音频的前提
  （`web/src/surfaces/realtime/index.tsx:293-320`）。

### 1.3 模型配置到面板的投影（providerType / surfaceKind / outputFormat.audio）

服务端可执行配置（`internal/modelcfg`）决定一个模型打开哪个 session surface：

1. `ProviderConfig.Type` 是 provider family；空值按 `llm` 处理
   （`internal/modelcfg/types.go:191-199`、
   `internal/modelcfg/types.go:58-63`）。
2. `SurfaceKindForType` 把 family 投影为 surface：`realtime → realtime`，
   `tts/stt → speech`，`image/video` 各自对应，未知 family 落到 `generic`
   （`internal/modelcfg/types.go:69-84`）。
3. `ProviderView` 和 `ModelView` 都同时携带 `providerType` 与 `surfaceKind`；
   遗留配置没有 family 时读作 `llm/llm`，不会读作 unknown
   （`internal/modelcfg/types.go:237-254`、
   `internal/modelcfg/types.go:261-273`、
   `internal/modelcfg/resolver.go:170-176`）。

客户端注册表的解析顺序是：

1. 显式的 `surfaceKind` / `providerType` 优先；
2. 没有显式类型时，才根据模型声明的 `outputFormat` / `inputFormat` 推断：
   - `outputFormat.audio && inputFormat.audio` → `realtime`；
   - 只有音频输出 → `speech`；
   - 只有音频输入且没有文本输出 → `speech`；
3. 都没有则回落到 `llm`。

证据：`web/src/surfaces/registry.ts:10-19`、
`web/src/surfaces/registry.ts:143-158`、
`web/src/surfaces/registry.ts:169-204`。

注意两个容易误判的点：

- `outputFormat.audio` 只是**没有显式 providerType 时的兜底推断**。一旦
  `providerType/surfaceKind` 存在，注册表按显式值解析，不再用音频能力猜面板
  （`web/src/surfaces/registry.ts:143-158`）。
- 一个模型要被 `modelcfg` 视为完整/可执行，`outputFormat.text` 必须为 true；
  纯音频输出模型会被标记为不完整
  （`internal/modelcfg/resolver.go:307-331`）。realtime 模型应声明
  `text: true` + `audio: true`，转写与语音两条路径都具备。
- `Selection` 里的 `providerType/surfaceKind` 是客户端 UI 提示，解析不会用它路由；
  它只是让选择器记住「这个 demo/新注册 Provider 该开哪个面板」
  （`internal/modelcfg/types.go:210-221`、
  `internal/modelcfg/overlay.go:337-341`）。

补充：`internal/modelprovider` 是 registry 层的声明契约（provider 类型、surface
descriptor、realtime transport 词汇），不是可执行配置层
（`internal/modelprovider/provider.go:1-24`）。它声明了 `webrtc` 这个 transport
词汇（`internal/modelprovider/provider.go:388-407`），但当前运行时的 realtime
桥只实现了 WebSocket，见 §8。

---

## 2. 环境变量与缺省行为

### 2.1 变量表

来源：`internal/realtime/provider.go:35-42`、`internal/realtime/provider.go:73-96`、
`internal/realtime/provider.go:100-147`。

| 变量 | 必填 | 语义 | 代码行为 |
| --- | --- | --- | --- |
| `KEEL_REALTIME_WS_URL` | **是** | Provider 的完整 WebSocket URL | 必须是 `ws://` 或 `wss://` 且有 host；为空 → `ErrNotConfigured`，`/v1/realtime` 503，`/status` `configured:false` |
| `KEEL_REALTIME_PROVIDER` | 否 | 协议 family：`qwen`（默认）或 `openai` | 大小写不敏感；空值默认 `qwen`；其他值让 Provider 构造失败，桥按未配置返回 503 |
| `KEEL_REALTIME_API_KEY` | 否 | 出站 `Authorization: Bearer <key>` | 为空且 `kind=qwen` 时回退 `DASHSCOPE_API_KEY`；**不会**回退 `OPENAI_API_KEY`；仍为空则不发送 Authorization |
| `DASHSCOPE_API_KEY` | 否 | 仅 qwen 的 key 兜底 | 只在 `KEEL_REALTIME_API_KEY` 为空且 `kind=qwen` 时读取 |
| `KEEL_REALTIME_MODEL` | 否 | 客户端未指定 model 时的默认 model | 同时参与允许列表构造；若显式 `KEEL_REALTIME_MODELS` 存在但不包含它，Provider 构造失败 |
| `KEEL_REALTIME_MODELS` | 否 | 允许的 model 列表 | 逗号/空格/制表符/换行分隔；列表内每一项必须完全匹配；空列表表示无策略 |

没有采样率环境变量。采样率由 `session.update` 的默认值或浏览器发送的
`session.update` 决定，见 §3。

### 2.2 解析顺序与允许列表

**有效 model 的解析顺序**（`internal/realtime/provider.go:211-225`）：

1. 客户端查询参数 `?model=...`；
2. `KEEL_REALTIME_MODEL`；
3. `KEEL_REALTIME_WS_URL` 自带的 `?model=...`。

解析出的 model 会写回出站 URL 的 `model` 查询参数，其他查询参数保留
（`internal/realtime/provider.go:229-237`）。

**允许列表的构造顺序**（`internal/realtime/provider.go:129-145`）：

1. `KEEL_REALTIME_MODELS`（显式列表）；
2. 否则 `KEEL_REALTIME_MODEL`；
3. 否则 URL 的 `?model=`；
4. 三者都没有 → 空允许列表 = 不限制，客户端 model 原样透传。

**校验规则**：

- 显式 `KEEL_REALTIME_MODELS` 非空时，客户端请求的 model 必须精确命中列表，否则
  `ValidateModel` 返回 `ErrModelNotAllowed`，HTTP 层在升级前返回 400
  （`internal/realtime/provider.go:190-193`、
  `internal/realtime/provider.go:211-219`、
  `internal/httpapi/realtime.go:102-108`）。
- 显式 `KEEL_REALTIME_MODELS` 非空且 `KEEL_REALTIME_MODEL` 也非空时，默认 model
  必须在允许列表中，否则 `NewProvider` 直接报错
  （`internal/realtime/provider.go:141-145`）。

### 2.3 配置示例（占位，不是可用性承诺）

Qwen 形态（URL 形态来自 `internal/realtime/provider.go:49-51` 的注释）：

```sh
KEEL_REALTIME_PROVIDER=qwen
KEEL_REALTIME_WS_URL=wss://<workspace>.<region>.maas.aliyuncs.com/api-ws/v1/realtime
KEEL_REALTIME_MODEL=<provider-model-id>
KEEL_REALTIME_MODELS=<provider-model-id>,<another-model-id>
KEEL_REALTIME_API_KEY=<server-side-key>
# 也可以用 DASHSCOPE_API_KEY 代替 KEEL_REALTIME_API_KEY（仅 qwen）
```

OpenAI 形态（URL 用占位符；实际地址以目标 Provider 文档为准）：

```sh
KEEL_REALTIME_PROVIDER=openai
KEEL_REALTIME_WS_URL=wss://<openai-realtime-endpoint>
KEEL_REALTIME_MODEL=<openai-realtime-model>
KEEL_REALTIME_API_KEY=<server-side-key>
```

### 2.2b demo provider（评审部署专用，默认关闭）

`KEEL_DEMO_PROVIDER=1` 时 `internal/realtime` 提供一个**进程内** realtime provider
（`internal/realtime/demo.go`）：不连任何上游、不需要厂商凭据、不读音频、不落任何用户
数据。它走与真实 provider 完全相同的路径（同一个 `RealtimeProvider` 接口、同一个
`Proxy` 桥、同一个 `/v1/realtime` WS 升级、同一套 `session.update` 注入与关闭码传播），
所以客户端没有任何 demo 分支。

```sh
KEEL_DEMO_PROVIDER=1
# 不需要 KEEL_REALTIME_WS_URL / KEEL_REALTIME_API_KEY
```

**选择顺序**（`FromLookup`，`internal/realtime/provider.go` 的文档注释；以
`TestFromLookupSelectionOrder` 逐条断言）：

| 序 | 判定条件（代码里的 switch 次序） | 结果 | 关键约束 |
| --- | --- | --- | --- |
| 1 | `KEEL_REALTIME_PROVIDER=demo` | demo provider（`NewDemoProvider()`） | 即使 `KEEL_REALTIME_WS_URL` 已配置也归 demo；若 `KEEL_DEMO_PROVIDER` 未开 → **配置错误**（`realtime: KEEL_REALTIME_PROVIDER=demo requires KEEL_DEMO_PROVIDER to be enabled`），**不回落厂商** |
| 2 | `KEEL_REALTIME_PROVIDER` 为其它 family（`qwen`/`openai`） | 该 family 的 WS provider，由 `KEEL_REALTIME_WS_URL` 构造 | 缺 URL → `ErrNotConfigured`；**显式 family 永不落到 demo** |
| 3 | 未设显式 family，但 `KEEL_REALTIME_WS_URL` 非空 | WS provider（family 默认 `qwen`） | **已配置的厂商端点优先于已启用的 demo**（打开 demo 开关不会替掉在跑的语音服务） |
| 4 | `KEEL_DEMO_PROVIDER` 开启且无厂商端点 | demo provider | 与 mediagw 的 demo 兜底同向 |
| 5 | 以上都不成立 | `ErrNotConfigured` | `GET /v1/realtime` 503、`/status` `configured:false`（既有行为） |

1. `KEEL_REALTIME_PROVIDER=demo` → demo provider（即使 `KEEL_REALTIME_WS_URL` 已配置）；
   若 `KEEL_DEMO_PROVIDER` 未开，这是**配置错误**（报错，不回落到厂商）。显式钉一个
   未启用的 provider 不能静默改服务另一个。
2. `KEEL_REALTIME_PROVIDER` 是别的 family（`qwen`/`openai`）→ 该 family 的 WS provider
   由 `KEEL_REALTIME_WS_URL` 构造；缺 URL 就是 `ErrNotConfigured`。**显式 family 永不
   落到 demo。**
3. 只配了 `KEEL_REALTIME_WS_URL`（没有显式 family）→ WS provider。**已配置的厂商端点
   优先于已启用的 demo**，所以打开 demo 开关不会悄悄替掉一个在跑的语音服务。
4. `KEEL_DEMO_PROVIDER` 开启且没有厂商端点 → demo provider。
5. 都不满足 → `ErrNotConfigured`（既有行为）。

**demo 事件序列**（`internal/realtime/demo.go`；`TestDemoTurnFrameSequence` 逐帧断言）：

| 客户端发送 | demo 回复 |
| --- | --- |
| （socket 打开） | `session.created` |
| `session.update` | `session.updated` |
| `response.create`，或**首个** `input_audio_buffer.append` | `response.created` → `response.audio_transcript.delta` × N → `response.audio_transcript.done` → `response.done`（`status:"completed"`） |
| `response.cancel`（队列里还有未发出的帧） | 丢弃整轮未发帧，只发 `response.done`（`status:"cancelled"`） |
| `response.cancel`（没有进行中的轮次） | 不发任何帧（客户端按「打断」时可能裸发 cancel，这里不造错误行） |

- 帧在**一步内整体入队**、由读端拉取，没有 sleep、没有节流，所以时序确定、测试可
  逐帧断言（`TestDemoTurnQueuesWithoutPacing`）。
- 每个帧只带客户端**已经在读**的字段：`type`、`delta`、`transcript`、`response.status`
  （`web/host.js` 与 `web/src/surfaces/realtime/index.tsx`）；没有自创事件名。
- 只在**首个** `input_audio_buffer.append` 起一轮：麦克风是连续上行的，每片都回会变成
  停不下来的回复环。
- 转写文本与分片复用 `internal/demo` 的 `ReplyText` / `ReplyChunks`（与 demo LLM 同一句、
  同一节奏）。

**`/v1/realtime/status` 在 demo 被选中时**（`TestRealtimeStatusDemoProvider`）：

```json
{"configured":true,"provider":"demo","kind":"demo","models":["demo-realtime"],
 "inputSampleRate":16000,"outputSampleRate":24000}
```

采样率依据：demo 的 `DefaultSessionUpdate` 声明嵌套 `audio.input.format.sample_rate=16000` /
`audio.output.format.sample_rate=24000`，`realtimeSampleRates`
（`internal/httpapi/realtime.go`）**从这个默认文档派生**（不另写字面量，所以不会漂移）；
这与浏览器宿主对非 `openai` kind 的默认一致——`web/host.js` 的 `providerRates` 是
`inputRate: openai ? 24000 : 16000`、`outputRate: 24000`，而 status 报的 `kind:"demo"`
不是 `openai`。

**未开 demo 时**（`TestRealtimeStatusDemoOff`）：`configured:false`、`models:[]`、两个采样率
为 0，且 `GET /v1/realtime` 直接 503 `{"error":"realtime provider is not configured"}`——
客户端因此停在配置态，不会去开一个最终 1006 的 WebSocket。

注意：

- `KEEL_REALTIME_PROVIDER=openai` 时不会读取 `OPENAI_API_KEY`；必须显式提供
  `KEEL_REALTIME_API_KEY`（`internal/realtime/provider.go:85-88`）。
- 这些变量在**进程启动时固定**。handler 每次请求重新读环境，但进程环境不会热更新；
  改完配置要重启/重新部署 Keel 进程（`internal/httpapi/realtime.go:67-70`、
  `internal/realtime/provider.go:68-68`）。

### 2.4 缺省行为：503 与配置态

| 场景 | `GET /v1/realtime` | `GET /v1/realtime/status` | 浏览器 UI |
| --- | --- | --- | --- |
| `KEEL_REALTIME_WS_URL` 为空且 demo 关 | `503 {"error":"realtime provider is not configured"}` | `200`，`configured:false`，`provider/kind` 为空，`models:[]`，采样率为 0 | preflight 直接进入「未配置语音模型」，不开 WebSocket |
| `KEEL_REALTIME_WS_URL` 为空但 `KEEL_DEMO_PROVIDER=1` | 升级 WebSocket（demo provider） | `200`，`configured:true`，`provider/kind` 均为 `demo`，`models:["demo-realtime"]`，采样率 16000/24000 | 可以开始通话（见 §2.2b） |
| URL/协议/模型策略非法 | 同上 503（具体原因只进服务端日志） | 同上 `configured:false` | 同上 |
| 配置有效 | 升级 WebSocket | `200`，`configured:true`，带 `provider/kind/models/inputSampleRate/outputSampleRate` | 可以开始通话 |

证据：`internal/httpapi/realtime.go:88-101`、
`internal/httpapi/realtime.go:186-215`、
`internal/httpapi/realtime_test.go:343-370`、
`internal/httpapi/realtime_test.go:631-680`。

`/v1/realtime/status` 未配置时返回 200 而不是错误，是刻意的：UI 要在打开 socket
之前渲染真实配置态，而不是等一个 WebSocket 1006 再猜原因
（`internal/httpapi/realtime.go:29-31`、
`internal/httpapi/realtime.go:180-191`、
`web/host.js:34-42`）。

---

## 3. 协议差异：qwen 与 openai

### 3.1 两套 `session.update`

服务端的默认 `session.update` 有两套厂商形态，按 `KEEL_REALTIME_PROVIDER` 选择
（`internal/realtime/provider.go:200-205`）；另有 demo 形态（第三种，见下表末行）：

| kind | 输入 | 输出 | 默认 voice | 默认转写模型 | 额外请求头 |
| --- | --- | --- | --- | --- | --- |
| `qwen` | PCM / s16le / **16 kHz** / mono | PCM / **24 kHz** | `longanlingxin` | `qwen3-asr-flash-realtime` | 无 |
| `openai` | PCM16 / **24 kHz** | PCM16 / **24 kHz** | 默认包不含 voice；浏览器补 `alloy` | 默认包不含；浏览器补 `whisper-1` | `OpenAI-Beta: realtime=v1` |
| `demo` | PCM / s16le / **16 kHz** / mono | PCM / **24 kHz** | `demo` | `demo-stt` | 无（进程内，无上游） |

代码事实：

- qwen 默认 `session.update` 的嵌套形状：
  `internal/realtime/provider.go:239-243`。
- openai 默认 `session.update` 的扁平形状：
  `internal/realtime/provider.go:245-248`。
- openai 额外头：`internal/realtime/provider.go:173-175`。
- qwen 默认 voice 和转写模型在默认 JSON 中：
  `internal/realtime/provider.go:243`。

### 3.2 最小可复制 JSON

Qwen（`kind=qwen`，服务端默认，原样可复制）：

```json
{
  "type": "session.update",
  "session": {
    "modalities": ["text", "audio"],
    "turn_detection": { "type": "server_vad" },
    "audio": {
      "input": {
        "format": {
          "type": "pcm",
          "sample_rate": 16000,
          "sample_format": "s16le",
          "channels": 1,
          "packing": "interleaved",
          "channel_layout": "mono"
        }
      },
      "output": {
        "voice": "longanlingxin",
        "format": { "type": "pcm", "sample_rate": 24000 }
      }
    },
    "input_audio_transcription": { "model": "qwen3-asr-flash-realtime" }
  }
}
```

OpenAI（`kind=openai`，服务端默认，原样可复制）：

```json
{
  "type": "session.update",
  "session": {
    "modalities": ["text", "audio"],
    "input_audio_format": "pcm16",
    "output_audio_format": "pcm16",
    "turn_detection": { "type": "server_vad" }
  }
}
```

### 3.3 浏览器实际发送的 `session.update`

正常 web 流程里，浏览器在 socket 打开后**立即**发送自己的 `session.update`
（`web/host.js:1073-1076`），因此 Provider 通常看到的是浏览器版本，而不是服务端
默认版本。浏览器版本同时保留旧扁平字段和新的嵌套 `audio` 字段：

- 外层：`modalities:["text","audio"]`、`voice`、`instructions`、
  `input_audio_format:"pcm16"`、`output_audio_format:"pcm16"`、
  `input_audio_transcription:{model: transcriptionModel}`。
- `turn_detection`：`server_vad`，`threshold:0.5`、
  `prefix_padding_ms:300`、`silence_duration_ms:700`、
  `create_response:true`、`interrupt_response:true`。
- `audio.input.format`：
  - qwen：`{type:"pcm", sample_rate, sample_format:"s16le", channels:1,
    packing:"interleaved", channel_layout:"mono"}`；
  - openai：`{type:"audio/pcm", rate}`。
- `audio.output`：`{voice, format:{type:"pcm", sample_rate}}`（openai 为
  `{type:"audio/pcm", rate}`）。

完整构造在 `web/host.js:473-515`。默认参数在
`web/host.js:202-218`：

| 参数 | qwen 默认 | openai 默认 |
| --- | --- | --- |
| `inputSampleRate` | 16000 | 24000 |
| `outputSampleRate` | 24000 | 24000 |
| `voice` | `longanlingxin` | `alloy` |
| `transcriptionModel` | `qwen3-asr-flash-realtime` | `whisper-1` |

`providerKind` 的判定规则：显式非泛化值优先；`""` / `realtime` /
`realtime-audio` / `voice` / `voice-agent` / `generic` / `audio` 视为泛化值，
再按 `providerId`/`model` 是否匹配 `openai|gpt-|whisper|o1/o3/o4` 判为 openai，
否则 qwen（`web/host.js:184-197`）。显式 `inputSampleRate`/`outputSampleRate`
参数总是覆盖默认值（`web/host.js:202-218`）。

### 3.4 默认注入的时机

桥只在客户端**第一条消息**不是 `session.update` 时注入服务端默认
（`internal/realtime/bridge.go:159-177`）：

- 客户端第一条消息就是 `session.update` → 原样转发，不注入；
- 客户端第一条消息是别的（例如 `input_audio_buffer.append`）→ 先发默认
  `session.update`，再转发该消息；
- 如果客户端一条消息都不发，桥不会主动发送默认（注入点是 pump 读到第一条客户端
  消息时）；
- 注入事件在日志里标记 `injected:true`，只记录 type/bytes，不记录内容
  （`internal/realtime/bridge.go:168-176`、
  `internal/httpapi/realtime.go:158-168`）。

### 3.5 `/v1/realtime/status` 的采样率来源

`realtimeSampleRates` 先假定 24000/24000，再尝试解析
`DefaultSessionUpdate()` 里的嵌套 `session.audio.input/output.format.sample_rate`
（`internal/httpapi/realtime.go:253-289`）：

- qwen 默认包有嵌套字段 → 返回 16000/24000（`internal/httpapi/realtime_test.go:579-586`）；
- openai 默认包只有扁平 `input_audio_format/output_audio_format`，没有嵌套数值 →
  返回 24000/24000 的默认值（`internal/httpapi/realtime.go:257-289`）。

因此「改默认 `session.update` 会同步反映到 status」只对**嵌套 audio 形状**成立；
openai 的扁平形状不参与解析。

---

## 4. 服务端桥契约

### 4.1 路由与鉴权

路由注册（`internal/httpapi/server.go:163-164`）：

```text
GET /v1/realtime/status
GET /v1/realtime
```

鉴权边界（`cmd/keel/auth.go`）：

- `/v1/realtime` 和 `/v1/realtime/status` 不在 auth 豁免列表里，必须经过
  auth middleware（`cmd/keel/auth.go:52-63`）。
- 配置了 `KEEL_SERVE_PASSWORD` 时：支持 `Authorization: Bearer <password>` 或
  `keel_session` cookie；未认证返回 401
  （`cmd/keel/auth.go:121-143`、`cmd/keel/auth.go:145-159`）。
- 认证启用但 `KEEL_SERVE_TENANT` 未配置：503
  （`cmd/keel/auth.go:131-135`）。
- middleware 会删除客户端自带的 `X-Tenant-ID`，注入服务端配置的 tenant
  （`cmd/keel/auth.go:137-141`）。
- 认证禁用时只接受回环 Host；在回环上仍会注入配置的 tenant
  （`cmd/keel/auth.go:96-118`）。

因此浏览器不需要、也不应该自己发 `X-Tenant-ID`；直接调用 API 的运维脚本在
认证关闭且未配置 tenant 时才需要自己发这个头。

注意：下面两节的 handler 行为都以请求已通过 auth middleware 为前提。认证启用但
`KEEL_SERVE_TENANT` 未配置时，middleware 会先返回 503；未认证会先返回 401
（`cmd/keel/auth.go:121-143`）。

### 4.2 `GET /v1/realtime/status`

Handler：`internal/httpapi/realtime.go:180-192`。

- tenant 缺失 → `401 {"error":"X-Tenant-ID is required"}`。
- 否则永远 `200`，body 字段固定：

```json
{
  "configured": true,
  "provider": "qwen",
  "kind": "qwen",
  "models": ["<model-id>"],
  "inputSampleRate": 16000,
  "outputSampleRate": 24000
}
```

未配置时：

```json
{
  "configured": false,
  "provider": "",
  "kind": "",
  "models": [],
  "inputSampleRate": 0,
  "outputSampleRate": 0
}
```

字段定义：`internal/httpapi/realtime.go:42-55`。构造逻辑：
`internal/httpapi/realtime.go:194-215`。允许列表镜像：
`internal/httpapi/realtime.go:217-236`。采样率解析：
`internal/httpapi/realtime.go:253-289`。

安全约束：body 永远不含 Provider URL、key、Authorization，也不含服务端错误正文
（`internal/httpapi/realtime.go:202-207`、
`internal/httpapi/realtime_test.go:579-586`、
`internal/httpapi/realtime_test.go:631-680`）。

### 4.3 `GET /v1/realtime`（WebSocket 升级）

Handler：`internal/httpapi/realtime.go:72-178`。处理顺序：

1. **tenant**：`X-Tenant-ID` 缺失或不是 UUID → 401
   （`internal/httpapi/realtime.go:74-78`）。
2. **session_id**：缺失/非 UUID → 400
   （`internal/httpapi/realtime.go:79-83`）。
3. **Provider 配置**：`realtime.FromEnv()` 失败 → 503
   `{"error":"realtime provider is not configured"}`；具体原因只写日志
   （`internal/httpapi/realtime.go:88-101`）。
4. **model 校验**：请求的 `model` 不在允许列表 → 400
   （`internal/httpapi/realtime.go:102-108`）。
5. **租户边界**：用 `LoadEvents` 确认 session 属于该 tenant；session 不存在/不属于
   → 404；事件读取 seam 未接线 → 503，绝不「先升级再说」
   （`internal/httpapi/realtime.go:110-122`、
   `internal/httpapi/server.go:574-595`）。
6. **升级 WebSocket**：`realtime.Accept`；Origin 使用库默认的同源检查
   （`internal/realtime/ws.go:17-29`）。
7. **连接 Provider**：15 秒超时；失败时先发结构化错误事件，再以 1011 关闭
   （`internal/httpapi/realtime.go:33-35`、
   `internal/httpapi/realtime.go:135-149`）。
8. **双向代理**：`realtime.Proxy` 原样搬运消息，只在日志里记录事件 type/bytes
   （`internal/httpapi/realtime.go:151-177`、
   `internal/realtime/bridge.go:1-17`）。

请求参数只有：

```text
GET /v1/realtime?session_id=<uuid>&model=<model-id>
```

`provider_id` 即使被浏览器带上，服务端也不会读取
（`web/host.js:333-342`、`internal/httpapi/realtime.go:79-83`、
`internal/httpapi/realtime.go:102-108`）。Provider URL/API key 只能来自进程环境
（`internal/httpapi/realtime.go:85-88`）。

### 4.4 桥内部行为

- **不解析音频**：payload 原样转发；只提取顶层 `type` 用于日志
  （`internal/realtime/bridge.go:1-17`、`internal/realtime/bridge.go:205-220`）。
- **默认 `session.update` 注入**：见 §3.4。
- **消息大小上限**：1 MiB；WebSocket 读限制和桥内检查都执行
  （`internal/realtime/ws.go:11-15`、
  `internal/realtime/bridge.go:154-167`）。
- **关闭码映射**：
  - 对端正常关闭 → 传播其 close code；
  - 1005 → 1000；
  - 1004/1005/1006/1015 不会出现在线上；不可发送的 code → 1011；
  - 超大消息 → 1009；
  - context 取消 → 1001；
  - 其他传输错误 → 1011。
  证据：`internal/realtime/bridge.go:116-151`、
  `internal/realtime/bridge.go:227-262`。
- **peer close reason 不进入错误字符串**：`CloseError.Error()` 只显示 code
  （`internal/realtime/realtime.go:82-94`）；HTTP 日志进一步把 reason 脱敏/截断
  （`internal/httpapi/realtime.go:309-329`）。
- **tenant/session 不下发**：Provider 的 `Connect` 忽略 `Session`
  （`internal/realtime/provider.go:160-164`）；出站只带 Authorization、OpenAI-Beta
  和 URL 的 model 查询参数。

### 4.5 客户端 preflight 流程

Surface 的 `startCall` 在开 socket **之前**先问配置态
（`web/src/surfaces/realtime/index.tsx:717-762`）：

```text
按「开始实时对话」
  → phase=connecting「正在检查实时语音服务…」
  → host.call("keel.realtime.preflight")
      → 同源 GET /v1/realtime/status（credentials: include，4 秒上限）
  → 四种结果：
      configured:false → phase=no-provider，直接结束，不开 WebSocket
      unsupported     → phase=unsupported，说明宿主没有音频桥
      unknown         → 让 start 自己说话（可能是旧宿主/临时网络问题）
      configured:true → 把 kind / inputSampleRate / outputSampleRate 交给 start
  → host.call("keel.realtime.start", ...)
      → WebSocket GET /v1/realtime?session_id=…&model=…
```

代码证据：

- preflight 的 host 调用和 4 秒超时：`web/host.js:898-978`
  （`PREFLIGHT_TIMEOUT_MS = 4000` 在 `web/host.js:303-305`）。
- surface 的 preflight 分类：`web/src/surfaces/realtime/index.tsx:658-715`。
- 未配置 → 不开 socket：`web/src/surfaces/realtime/index.tsx:742-746`；
  测试也断言这一点：`web/src/surfaces/realtime/realtime.test.tsx:342-353`。
- preflight 不可用时的兼容：`web/src/surfaces/realtime/index.tsx:641-644`
  （`preflightMissing`）和 `web/src/surfaces/realtime/index.tsx:672-674`。

### 4.6 错误矩阵

| 场景 | HTTP / WS 结果 | 代码证据 |
| --- | --- | --- |
| 无 tenant | `401 X-Tenant-ID is required` | `internal/httpapi/realtime.go:74-78` |
| `session_id` 非 UUID | `400 bad session_id` | `internal/httpapi/realtime.go:79-83` |
| Provider 未配置/配置非法 | `503 {"error":"realtime provider is not configured"}` | `internal/httpapi/realtime.go:88-101` |
| model 不在允许列表 | `400 realtime model is not allowed: …` | `internal/httpapi/realtime.go:102-108`、`internal/realtime/provider.go:190-193` |
| session 不存在/不属于 tenant | `404 {"error":"session not found"}` | `internal/httpapi/server.go:574-580`、`internal/httpapi/realtime_test.go:413-436` |
| 事件 seam 未接线 | `503 contract kernel is not wired for the edge endpoints` | `internal/httpapi/realtime.go:114-122`、`internal/httpapi/realtime_test.go:545-576` |
| 升级后 Provider 连接失败 | 先发 `{"type":"error","error":{"type":"provider_unavailable",…}}`，再 close 1011 | `internal/httpapi/realtime.go:135-149`、`internal/httpapi/realtime.go:291-307` |
| 单条消息 > 1 MiB | close 1009 | `internal/realtime/bridge.go:165-167`、`internal/realtime/bridge.go:244-248` |

---

## 5. 浏览器宿主契约（`web/host.js`）

### 5.1 函数表

`web/host.js:109-133` 暴露以下 `keel.realtime.*` 调用（`nsh.json` 的
`capabilities.host: ["keel.*"]`，`web/nsh.json:25-27`）：

| 调用 | 入参 | 返回 | 关键行为 |
| --- | --- | --- | --- |
| `keel.realtime.start` | `{sessionId, model, providerKind?, providerId?, instructions?, voice?, inputSampleRate?, outputSampleRate?, transcriptionModel?}` | `{ok:true, mic:"granted" / "not-allowed" / "no-device" / "mic-failed" / "mic-timeout" / "unsupported", message}` 或 `{ok:false, code, message}` | 建立 socket、发 `session.update`、申请麦克风、启动采集/播放；同一时刻最多一个会话，第二个 `start` 结束第一个 |
| `keel.realtime.preflight` | 无（surface 可能带 sessionId/model，host 忽略） | `{ok:true, configured, kind?, models?, inputSampleRate?, outputSampleRate?}` 或 `{ok:false, code:"preflight-failed"|"unsupported", message}` | 同源 GET `/v1/realtime/status`；4 秒上限；不泄露 URL/key/服务端错误正文 |
| `keel.realtime.send` | `{payload: object}`（Provider JSON 事件） | `{ok:true}` 或 `{ok:false, code, message}` | 只在 socket OPEN 时发送；`response.cancel` 会设置取消门并停止本地播放 |
| `keel.realtime.mute` | `{muted: boolean}` | `{ok:true, muted}` 或 `{ok:false, code, message}` | 切换麦克风 track 的 `enabled`；不影响助手音频播放 |
| `keel.realtime.stop` | 无 | `{ok:true, wasLive: boolean}` | 结束会话，释放麦克风、播放队列、AudioContext 和 socket |
| `keel.realtime.status` | 无 | `{ok:true, live:true, ready, muted, attempts, providerKind, inputRate, outputRate}` 或 `{ok:true, live:false}` | 只读诊断；不是产品 UI 必需 |

证据：`web/host.js:109-133`、`web/host.js:980-1193`。

`start` 的必需参数只有 `sessionId` 和 `model`；没有 model 返回
`{ok:false, code:"bad-request", message:"没有指定实时模型…"}`
（`web/host.js:980-997`）。`sessionId` 会被拼进 `/v1/realtime?session_id=…`
（`web/host.js:333-342`）。

### 5.2 事件表

宿主通过 `player.hostEvent(name, payload)` 向 app 发三类实时事件：

| 事件名 | Payload | 触发时机 | 证据 |
| --- | --- | --- | --- |
| `keel.realtime.state` | `{phase, detail, at, ...extra}` | 连接中/就绪/说话/思考/打断/重连/错误/结束等阶段变化 | `web/host.js:326-328` |
| `keel.realtime.event` | `{event: <Provider JSON>}` | 每一条能解析为 JSON 的 Provider 消息；`response.audio.delta` 的 base64 被剥离，只留 `audioStripped:true` 和 `audioBytes` | `web/host.js:602-617`、`web/host.js:281-290` |
| `keel.realtime.level` | `{input, output}`，0..1 | 会话就绪后每 100 ms 一次，输入/输出 RMS 电平 | `web/host.js:791-798`、`web/host.js:1115` |

`keel.realtime.state` 的 `phase` 由 host 发出的值包括：
`connecting`、`ready`、`thinking`、`speaking`、`interrupted`、`reconnecting`、
`error`、`closed`（`web/host.js:1041`、`web/host.js:449-451`、
`web/host.js:466-471`、`web/host.js:540-542`、`web/host.js:555-557`、
`web/host.js:560-563`、`web/host.js:637-640`、`web/host.js:658-665`、
`web/host.js:875-877`）。

`listening` 不是 host 发出的阶段；它由 surface 在收到
`input_audio_transcription.delta` 时设置
（`web/src/surfaces/realtime/index.tsx:297-301`）。surface 还有
`permission-denied`、`unsupported`、`no-session`、`no-provider` 等宿主不知道的
阶段（`web/src/surfaces/realtime/components/status-strip.tsx:15-32`）。

### 5.3 真实 Provider 事件名与归约

Host 自己处理的事件（`web/host.js:526-567`）：

| Provider 事件 | Host 行为 |
| --- | --- |
| `session.created` / `session.updated` | 标记 ready；重置重连预算 |
| `input_audio_buffer.speech_started` | 打断：停止播放、取消 response、state=`interrupted` |
| `response.created` | 取消门解除，标记 responseActive，state=`thinking` |
| `response.audio.delta` | 解码/调度播放；首次出现时 state=`speaking` |
| `response.audio.done` | 不做事（取消中的 turn 保持停止） |
| `response.done` | 结束本轮；若未取消且正在说话 → state=`ready` |
| `error` | state=`error`，`recoverable:true`；socket 尚未打开时 `attachSocket` 以该 reason 失败；socket 已打开但未 ready 时只记录 `lastReason`，交给后续 ready 宽限/关闭路径 |

Surface 在 `keel.realtime.event` 上进一步归约的事件
（`web/src/surfaces/realtime/index.tsx:293-424`）：

- `*.input_audio_transcription.delta` / `.completed` / `.failed`；
- `input_audio_buffer.speech_started` / `.stopped`；
- `response.created`、`response.done`；
- `response.audio_transcript.delta` / `.done`、`response.text.delta` / `.done`；
- `response.audio.delta` / `.done`；
- `response.output_item.added` / `.done`、`response.function_call_arguments.delta` / `.done`；
- `error` / `session.error`。

这说明事件名称来自代码的 `endsWith`/精确匹配，不是文档臆造。
`response.audio.delta` 的音频只到 host 为止；app 看到的是去掉 base64 的事件
（`web/host.js:281-290`、`web/host.js:609-611`）。

### 5.4 生命周期、超时与默认值

- **socket 打开超时**：8 秒（`web/host.js:587-594`）。
- **等待 Provider session 事件（ready）的宽限**：6 秒；超时按 ready 处理
  （`web/host.js:800-829`）。
- **麦克风授权超时**：20 秒（`web/host.js:1091-1097`）。
- **重连**：最多 3 次；第 n 次延迟 `400 * n` ms；每次重连都发一次新的
  `session.update`。如果 800 ms 内没有收到 Provider 的 session 事件，宽限
  `markReady` 会带上 `reconnected:true`，并明确告知这是一段新 Provider 会话、
  断线前的转写不会重放（`web/host.js:300-314`、`web/host.js:647-686`、
  `web/host.js:673-679`）。
- **不可重连的关闭**：close code `4400/4401/4403/4404/4408`，或 close reason
  命中「not configured / invalid key / unauthorized / invalid model / 未配置 /
  未授权 / 鉴权 / 密钥」等模式
  （`web/host.js:300-314`）。
- **页面隐藏/关闭**：`visibilitychange` 到 hidden 或 `pagehide` 会主动结束通话，
  避免后台继续监听（`web/host.js:169-177`）。
- **单会话**：`start` 时如果已有 session，先 `end("上一次实时会话已被新的会话替换")`
  （`web/host.js:997`）。
- **麦克风约束**：单声道、回声消除、降噪、自动增益
  （`web/host.js:693-697`）。
- **采集**：`ScriptProcessor(4096,1,1)`，重采样到 `inputRate`，再 base64 成
  PCM16 后发 `input_audio_buffer.append`；`muted` 或尚未 `ready` 时不发
  （`web/host.js:731-763`）。
- **播放**：按 `outputRate` 创建 AudioBufferSourceNode，排程在
  `max(currentTime+0.02, nextPlayAt)`，避免重叠/爆音
  （`web/host.js:406-452`）。
- **默认 instructions**：`你是 Keel 的实时语音助手。回答简短、口语化，需要时再展开。`
  （`web/host.js:1020-1022`）。

---

## 6. 验收步骤

### 6.1 前置检查

1. 启动带 web 构建的 `keel serve`，确保：
   - `KEEL_SERVE_PASSWORD` / `KEEL_SERVE_TENANT`（或回环开发模式）已配置；
   - 浏览器能打开同源 web UI；
   - 数据库/事件 seam 已接线（否则 `/v1/realtime` 会 503，见 §4.6）。
2. 配置 `KEEL_REALTIME_*` 并重启 Keel 进程。
3. 用运维脚本查状态：

```sh
curl -sS -H "Authorization: Bearer $KEEL_SERVE_PASSWORD" \
  http://127.0.0.1:13750/v1/realtime/status
```

（`127.0.0.1:13750` 是 `KEEL_SERVE_ADDR` 的默认值，见
`cmd/keel/main.go:64`。）

期望 `configured:true`，且 `kind`、`models`、`inputSampleRate`、
`outputSampleRate` 与实际 Provider 配置一致。

### 6.2 未配置状态验收（先验收失败路径）

临时清空 `KEEL_REALTIME_WS_URL` 并重启，或在没有配置的实例上：

1. `GET /v1/realtime/status` 返回 `200`，`configured:false`，`models:[]`。
2. 在已打开 session、且已选中一个 realtime 模型的前提下，打开 realtime surface，
   预期：
   - 状态条显示「未配置语音模型」(`no-provider`)；
   - 如果模型注册表里也没有 realtime 模型，主按钮「开始实时对话」禁用，并显示
     「打开模型设置」；
   - 如果模型存在但服务端未配置，按钮可点；点一次后经历「连接中」→
     「未配置语音模型」，并出现「打开模型设置」；
   - **DevTools Network 里不应出现 `/v1/realtime` WebSocket**；只有
     `/v1/realtime/status` 的 GET。
3. 空态文案应显示「麦克风持续收音、助手语音边说边播，不是录音后再转写」
   （`web/src/surfaces/realtime/index.tsx:1088-1097`）。

证据：preflight 未配置分支 `web/src/surfaces/realtime/index.tsx:742-746`；
测试断言 `web/src/surfaces/realtime/realtime.test.tsx:342-353`；
状态条文案 `web/src/surfaces/realtime/components/status-strip.tsx:100-104`。

### 6.3 一次正常通话

1. 选择一个 `providerType=realtime`/`surfaceKind=realtime` 的模型，打开一个真实
   session；按「开始实时对话」。
2. 预期 UI 依次出现：

| 阶段 | 状态条 | 发生什么 |
| --- | --- | --- |
| 检查服务 | 连接中 | surface 在 `preflight`，此时还没有 WebSocket |
| 建连/配置 | 连接中 | host 打开 `/v1/realtime`，发 `session.update`，申请麦克风 |
| 就绪 | 就绪 | 收到 `session.created`/`session.updated`；麦克风徽标「麦克风已授权」；计时器开始 |
| 用户说话 | 聆听 | 收到转写 delta；底部出现 dimmed partial 行 |
| 一轮结束 | 思考 | 收到 `input_audio_transcription.completed` / `response.created` |
| 助手说话 | 说话 | 收到 `response.audio.delta` 和/或 transcript delta；音频播放 |
| 本轮结束 | 就绪 | 收到 `response.done`，提示「这一轮结束，可以继续说下一句」 |
| 结束通话 | 已结束 | 麦克风释放、播放停止、计时器归零 |

阶段标签与提示文案：`web/src/surfaces/realtime/components/status-strip.tsx:18-105`。
事件归约：`web/src/surfaces/realtime/index.tsx:293-424`、
`web/src/surfaces/realtime/index.tsx:446-512`。

注意：「聆听」依赖 Provider 的 `input_audio_transcription.*` 事件
（`web/src/surfaces/realtime/index.tsx:297-306`）；如果某个模型不发送转写事件，
用户说话时状态条可能仍停在「就绪」，但麦克风上行和 VAD 仍然工作。

3. 说话时观察：
   - 麦克风输入电平条在动（`keel.realtime.level`，`web/host.js:791-798`）；
   - 用户最终转写成为一条 user row，partial 行被替换而不是重复
     （`web/src/surfaces/realtime/index.tsx:303-306`）；
   - 助手回复是 assistant row，转写流式增长；音频与文字属于同一轮
     （`web/src/surfaces/realtime/index.tsx:321-348`）。

### 6.4 打断验收

1. 在助手「说话」阶段开口，或点「打断」。
2. 预期：
   - 状态条变为「已打断」；
   - 播放立即停止；后续 `response.audio.delta` 不再播放
     （`web/host.js:406-410`、`web/host.js:543-545`）；
   - 当前 assistant row 标记为 `interrupted`，不会被迟到的 `response.done`
     补成完整回复（`web/src/surfaces/realtime/index.tsx:386-414`）；
   - 若当前有进行中的 response，`speech_started` 会触发 `response.cancel`；
     手动「打断」则总会发送取消事件
     （`web/host.js:455-471`、`web/src/surfaces/realtime/index.tsx:864-874`）。
3. 如果 Provider 仍继续发音频：宿主用 `cancelGate` 丢弃，直到下一次
   `response.created` 才解除（`web/host.js:536-545`）。

### 6.5 结束与失败验收

- 点「结束通话」：状态条「已结束」，麦克风轨道停止，WebAudio context 关闭，
  socket 以 1000 关闭（`web/host.js:832-877`、`web/host.js:1164-1168`）。
- 切到后台/关闭页面：宿主主动结束，reason 为「页面已切到后台/已关闭」
  （`web/host.js:169-177`）。
- Provider 返回 `error`：状态条「出错」，transcript 里出现
  「实时会话错误：<message>」；宿主记录 `lastReason`。`error` 事件本身不触发
  重连；只有 socket 随后关闭且不是致命原因时，才进入重连策略
  （`web/host.js:559-563`、`web/host.js:611-617`、
  `web/src/surfaces/realtime/index.tsx:416-423`）。
- Provider 连接失败（已升级）：先收到
  `{"type":"error","error":{"type":"provider_unavailable",...}}`，随后 close 1011
  （`internal/httpapi/realtime.go:135-149`、`internal/httpapi/realtime.go:291-307`）。

### 6.6 UI 状态对照表

| `RealtimePhase` | 状态条文案 | 典型来源 |
| --- | --- | --- |
| `idle` | 未开始 | 初始 |
| `connecting` | 连接中 | preflight / start |
| `ready` | 就绪 | `session.created`，或 6 秒宽限 |
| `listening` | 聆听 | 转写 delta |
| `thinking` | 思考 | 转写完成 / `response.created` |
| `speaking` | 说话 | `response.audio.delta` |
| `interrupted` | 已打断 | `speech_started` / 手动打断 / 取消的 `response.done` |
| `reconnecting` | 重连中 | 非致命 close |
| `error` | 出错 | Provider `error` / 非致命重连耗尽 |
| `closed` | 已结束 | stop / 页面隐藏 |
| `permission-denied` | 麦克风未授权 | 麦克风被拒绝，文字兜底 |
| `unsupported` | 宿主不支持 | 没有 WebSocket/AudioContext/preflight 调用 |
| `no-session` | 未打开会话 | 没有 active session |
| `no-provider` | 未配置语音模型 | 没有 realtime 模型，或服务端未配置 |

证据：`web/src/surfaces/realtime/components/status-strip.tsx:18-105`、
`web/src/surfaces/realtime/index.tsx:599-609`。

---

## 7. 故障排查表

| 现象 | 最可能原因 | 检查 | 修复 | 代码证据 |
| --- | --- | --- | --- | --- |
| WebSocket 以 **1006** 关闭，reason 为空 | 升级前 HTTP 被拒（401/403/404/503），或连接未收到 close frame 就断了 | 先看 `/v1/realtime/status`；看 Network 里 `/v1/realtime` 的 HTTP 状态；看 Keel 日志 `realtime upgrade rejected` / `realtime provider connect failed`；确认反向代理允许 Upgrade/Connection 头 | 修好认证/tenant/session/Provider 配置，或修代理；preflight 未配置时不要开 socket | `internal/realtime/bridge.go:227-262`、`internal/httpapi/realtime.go:124-149`、`web/host.js:619-643` |
| 声音变调（音高/速度不对） | 采样率不匹配：上行重采样率、下行播放率与 Provider 实际 `session.update` 格式不一致 | 对比 `/v1/realtime/status` 的 `inputSampleRate/outputSampleRate`、浏览器发出的 `session.update`、Provider 的 `session.updated`；确认 `KEEL_REALTIME_PROVIDER` 与前端 `providerKind` 一致 | 让 `providerKind` 与 Provider family 一致；显式传 `inputSampleRate/outputSampleRate`；确保 16k/24k（qwen）或 24k/24k（openai）一致 | `web/host.js:202-218`、`web/host.js:429-445`、`web/host.js:748-758`、`internal/httpapi/realtime.go:253-289` |
| 打断后还有尾音 | 已经排进 WebAudio 的音频、系统音频缓冲、或 Provider 未真正停止发送 | 看打断后是否还有 `response.audio.delta`；看 `cancelGate` 是否在下一个 `response.created` 才解除；看 VAD `silence_duration_ms=700` 的延迟 | 确认 Provider 支持 `response.cancel`；若尾音极短属于设备缓冲；不要把 `response.audio.done` 当成停止 | `web/host.js:392-410`、`web/host.js:455-471`、`web/host.js:536-545`、`web/src/surfaces/realtime/index.tsx:864-874` |
| 麦克风权限被拒绝 | 浏览器 `NotAllowedError`/`SecurityError`/`PermissionDeniedError` | 看返回 `mic` 是否为 `not-allowed`；看浏览器站点权限 | 在浏览器设置里授权；未授权时会话仍可用文字输入，助手语音仍播放 | `web/host.js:688-729`、`web/host.js:1117-1123`、`web/src/surfaces/realtime/index.tsx:819-833` |
| Provider `error` 事件 / 会话建立前失败 | 凭据错误、模型不存在、额度/配额、Provider 拒绝 | 看 `start` 返回的 `code/message`；看服务端 `realtime provider connect failed` 日志；看事件流里的 `error.error` | 修 Provider key/model/额度；不可重连的 code 不会重试，需要改配置后重启通话 | `web/host.js:559-563`、`web/host.js:611-617`、`internal/httpapi/realtime.go:135-149`、`internal/httpapi/realtime.go:291-307` |
| 刷新后转写全丢 | 当前构建不写账本，rows 只在页面信号里 | 确认刷新前没有持久化提示；看 `realtime-persistence-note` | 这是已知限制，不是故障；需要持久化要另做账本写入 | `web/src/surfaces/realtime/index.tsx:20-25`、`web/src/surfaces/realtime/index.tsx:1216-1228` |
| 重连后断线前的转写不见了 | 重连是新的 Provider 会话，不会 replay | 看状态条是否提示「已重连…断线前的转写不会重放」 | 预期行为；本地 rows 仍在，Provider 侧不恢复 | `web/host.js:647-686`、`web/src/surfaces/realtime/index.tsx:456-458` |
| 400 `realtime model is not allowed` | 前端选择/发送的 model 不在 `KEEL_REALTIME_MODELS`/`KEEL_REALTIME_MODEL`/URL model 里 | 对比 `/v1/realtime/status` 的 `models` 与 `start` 的 `model` | 对齐模型 id，或补 `KEEL_REALTIME_MODELS` | `internal/realtime/provider.go:211-219`、`internal/httpapi/realtime.go:102-108` |
| 宿主不支持 / 按钮不可用 | 非 web 构建、没有 WebSocket/AudioContext/preflight | 看 phase `unsupported` 和 `host.call` 错误 | 用带浏览器音频桥的 web 构建；或改用文字 | `web/src/surfaces/realtime/index.tsx:740-751`、`web/src/surfaces/realtime/index.tsx:780-795` |

补充：服务端日志只记录事件 `type` 和 `bytes`，不记录 base64 音频或转写文本
（`internal/httpapi/realtime.go:158-168`、
`internal/realtime/bridge.go:29-40`）。排查时不要指望日志里有音频内容。

---

## 8. 当前未覆盖

### 8.1 真实厂商实时凭据未接入到模型配置

realtime 桥只通过 `realtime.FromEnv()` 读进程环境（`KEEL_REALTIME_*`）
（`internal/httpapi/realtime.go:67-70`、
`internal/realtime/provider.go:68-96`）。它**不读取**：

- `internal/modelcfg` 的 `ProviderConfig.Access`（env/ref）或租户密钥
  （`internal/modelcfg/types.go:125-129`、`internal/modelcfg/types.go:191-199`）；
- `internal/modelprovider` 的 provider instance / `CredentialRef`
  （`internal/modelprovider/provider.go:649-665`）；
- 前端模型注册表中的 provider 凭据。

因此「在设置里配置了一个 realtime 模型」不等于「实时语音桥有可用凭据」；
后者必须通过部署环境注入。仓库内置 catalog 没有声明 `type: realtime` 的 provider，
全部按默认的 llm family 处理：`internal/modelcfg/catalog/builtin.json:5-110`。

### 8.2 WebRTC transport 未实现

`internal/modelprovider` 的声明词汇允许 realtime transport 为 `websocket` 或
`webrtc`（`internal/modelprovider/provider.go:388-407`、
`internal/modelprovider/provider.go:517-538`），但当前运行时：

- 服务端只有通用 WebSocket provider（`internal/realtime/provider.go:149-158`、
  `internal/realtime/ws.go:31-40`）；
- 浏览器宿主只 `new WebSocket(...)`（`web/host.js:569-594`）；
- `RealtimeProvider` 接口注释把 WebRTC 列为「未来插件」的扩展点，而不是已实现路径
  （`internal/realtime/realtime.go:68-77`）。

所以声明 `transport: webrtc` 的 Provider 不能被当前内置桥直接服务，需要插件实现
`RealtimeProvider` 或另做媒体服务器。

### 8.3 转写不持久化

realtime surface 明确说明：本构建不写账本，rows 只存在于页面投影中；刷新后不会从
账本恢复（`web/src/surfaces/realtime/index.tsx:20-25`、
`web/src/surfaces/realtime/index.tsx:1216-1228`）。final 只是「本页面内 final」，
不是持久化记录。

### 8.4 TTS 路由已实现，STT 仍走浏览器 Speech

（2026-11-14 按代码校正；本条旧表述为「TTS/STT 服务端路由未实现」，已被 change
`keel-demo-provider-v1` 的 S2c 与 S3 部分取代。）

- **TTS 服务端路由已实现**：`POST /v1/media/audio/speech`（`internal/httpapi/media_audio.go`，
  `registerMediaAudioRoutes`）。当前唯一的 provider 是进程内 demo（`KEEL_DEMO_PROVIDER=1`）；
  未启用即 503，body 不点名凭据或上游地址。
- **STT 服务端路由已实现**：`POST /v1/media/audio/transcriptions`（同一文件），上限 8 MiB
  multipart；同样由 demo provider 提供，未启用 503。
- **STT 面板仍走浏览器 `Speech` API，还没切到服务端**：`web/src/surfaces/speech/index.tsx`
  的 STT 半边走 `Speech.start`（zh-CN、interim），不经服务端 ASR。客户端函数
  `transcribeAudio`（`web/src/data/media.ts`）已就绪但**当前无调用点**——缺的是「麦克风音频
  采集 → Blob」这段桥接；接上后即可切换到 `/v1/media/audio/transcriptions`。
- **TTS 面板不再是硬编码 `disabled`**：面板挂载时探 `GET /v1/media/capabilities`
  （`mediaCapabilities()`），`tts: true` 时合成按钮按 `ttsState() === "ready"` 启用并调用
  `synthesizeSpeech`；`tts: false` 才显示配置态「TTS Provider 尚未配置」。

### 8.5 其他边界

- **重连不 resume**：每次重连都是新的 Provider 会话，不重放断线前的转写
  （`web/host.js:647-686`、`web/src/surfaces/realtime/index.tsx:456-458`）。
- **工具调用只展示不执行**：surface 的免责声明写明「本面板不执行工具调用」
  （`web/src/surfaces/realtime/index.tsx:1205-1214`）；工具事件只投影成 tool 行。
- **音频编码只实现 PCM16**：host 只编码 PCM16（`web/host.js:229-238`），
  虽然 registry 层声明了 pcm24/pcm32/opus/g711 词汇
  （`internal/modelprovider/provider.go:410-435`）；当前桥不处理这些编码。

---

## 附录：与旧版文档的口径差异

以下是本次按代码校正的几处旧文档表述（代码行号为当前工作树）：

1. **旧文档称前端只给 `start` 传 4 个参数**。实际 surface 还会把 preflight 得到的
   `kind/inputSampleRate/outputSampleRate` 透传给 `start`
   （`web/src/surfaces/realtime/index.tsx:752-773`）。
2. **旧文档称服务端默认 `session.update` 只在客户端「从未发送」时注入**。实际判定是
   第一条客户端消息：第一条不是 `session.update` 就注入，哪怕客户端稍后会发
   （`internal/realtime/bridge.go:159-177`）。
3. **旧文档称 `/status` 的采样率总是从默认 `session.update` 解析**。实际只有嵌套
   `session.audio.*.format.sample_rate` 会被解析；openai 的扁平默认没有数值字段，
   走 24000/24000 常量兜底（`internal/httpapi/realtime.go:257-289`）。
4. **旧文档称 TTS 按钮在「Provider 未配置时」禁用**。实际按钮无条件 `disabled`，
   且「未配置」是硬编码文案；没有配置成功后启用的路径
   （`web/src/surfaces/speech/index.tsx:847-856`、
   `web/src/surfaces/speech/index.tsx:459-466`）。
5. **旧文档把厂商实测矩阵写成当前可用性结论**。本版只保留代码契约：bridge 的
   URL/key 只来自进程环境，仓库内置 catalog 没有 `type: realtime` 的 provider；
   具体厂商/模型/额度必须在目标部署里重新验证
   （`internal/httpapi/realtime.go:67-70`、
   `internal/realtime/provider.go:68-96`、
   `internal/modelcfg/catalog/builtin.json:5-110`）。
