# Design: keel-demo-provider-v1

## 1. 边界

```
客户端                          服务端
──────                          ──────
/v1/models ──────────────────►  modelcfg: 租户文档 + 内置 catalog
   │                              └─ demo provider（仅当 KEEL_DEMO_PROVIDER=1）
   ├─ ProviderView.providerType
   └─ ModelView.providerType / surfaceKind
surface 装载（无 demo 分支）
   │
   ├─ llm      → POST /v1/sessions/{id}/input → modelprovider → demo driver
   ├─ image    → POST /v1/media/images/generate → mediagw → demo media provider
   ├─ decision → POST /v1/media/decision        → mediagw → demo decision
   ├─ realtime → GET  /v1/realtime (WS)         → realtime → demo WS provider
   └─ tts/stt  → POST /v1/media/audio/*         → mediagw → demo audio
```

客户端没有任何「这是演示」的分支：demo provider 若存在，它就是 provider；若不存在，surface
落配置态（已有）。

## 2. demo provider 契约

- 启用：`KEEL_DEMO_PROVIDER=1`（默认关）。生产不启用。
- 身份：providerId = `demo`；模型：
  - `demo-llm`（providerType=llm）
  - `demo-image`（image）
  - `demo-decision`（decision）
  - `demo-realtime`（realtime，surfaceKind=realtime）
  - `demo-tts`（tts，surfaceKind=speech）
  - `demo-stt`（stt，surfaceKind=speech）
- 每个模型 MUST 带 `inputFormat/outputFormat`，让现有能力门与 surface 解析原样工作。
- 内容：确定性、可复现（固定 seed 文案/固定尺寸图/固定转写），不含任何真实用户数据。

## 2.1 已裁决的路由规则（S2b 选 A：按显式模型 id 路由）

**规则原文**：

> 显式指定 `demo-*` 模型时由 demo provider 服务；不带 model 的默认路径仍 vendor 优先；
> 开关关闭时 demo 模型一律拒绝（有 vendor 时 400，无 vendor 时 503）。

三行对应三个判据，实现上是三个独立的布尔量，不能合并：

| 请求形态 | 开关 ON + 有 vendor | 开关 ON + 无 vendor | 开关 OFF |
|---|---|---|---|
| `model:"demo-image"`（显式 demo 家族） | demo 服务，vendor 不调用 | demo 服务 | 400（有 vendor，允许列表拒）/ 503（无 vendor） |
| 不带 `model`（部署默认） | vendor 服务（行为不变） | demo 兜底（历史 503 的唯一例外） | 503（既有行为） |
| `model:"<vendor 模型>"` | vendor 服务 | 400（demo 只服务自己的 id） | vendor 服务 / 503 |

**为什么选 A（而不是「vendor 优先覆盖 demo」）**：

1. **目录里列出的模型必须可服务。** `/v1/models` 在开关开启时列出六个 demo 模型，客户端就能
   选中它们。若路由仍是「有 vendor 就一律走 vendor」，混部部署会出现「能选中、一跑就 400
   (`model "demo-image" is not allowed`)」——一个只在有 vendor 凭据的机器上复现的产品缺陷。
   路由键取「请求显式钉了哪个 model」而不是「部署有没有 vendor」，可服务性才与目录一致。
2. **`/v1/models` 不应随 vendor 凭据漂移。** 若让「vendor 是否配置」决定目录内容（另一个候选
   方案：有 vendor 时不列 demo 模型），同一份 `KEEL_DEMO_PROVIDER=1` 配置在两台机器上会产出
   两份不同的目录；客户端的选择记忆、缓存与「我上次选的那个模型还在不在」的判断全部失效。
   开关是唯一开关，目录只由它决定。

**代价与边界**：这条规则让「不带 model」保留历史语义（部署默认 ＝ vendor），因此混部部署里
默认路径与显式路径可以指向不同 provider。这是刻意的：改默认路径会改变既有部署的行为，
而默认路径的用户没有做任何 demo 相关选择。

**实现落点**（同一判据在多处，必须同时正确）：

- `internal/mediagw/demo.go`：`demoEnabled`（只看开关）、`demoActive`（开关 + 无 vendor ＝
  默认兜底）、`demoRequested`（开关 + 显式家族匹配）。`mediagw.go` 的 `Generate` / `Edit` /
  `Decision` / `Chat` 四个方法都用 `demoRequested(...) || (!Configured && demoActive())` 同一表达式。
- `internal/realtime/provider.go`：`FromLookup` 的 5 条有序规则（显式 `=demo` ＞ 显式 family
  ＞ 厂商 URL ＞ demo 兜底 ＞ 未配置）。语音这条**显式钉了 demo 就赢**，与 mediagw 同向。
- `internal/httpapi/media_audio.go`：`audioModel` —— 空 model 取默认，等于 `demo-tts`/`demo-stt`
  才接受，别的 id 直接 400（不静默改路由到 demo）。
- 回归证据：`internal/mediagw/demo_test.go` 的 `TestDemoExplicitModelWinsOverVendor` /
  `TestDemoUnnamedRequestStaysVendor` / `TestDemoModelRefusedWhenSwitchOff` /
  `TestDemoVendorModelStillRejectedWithDemoOn`；`internal/httpapi/demo_assets_test.go` 的
  `TestDemoModelSelectableWithVendorConfigured` / `TestDemoModelRefusedWhenSwitchOff`；
  `internal/realtime/demo_test.go` 的 `TestFromLookupSelectionOrder`。

## 3. 服务端实现要点

- **LLM**：`internal/modelprovider` 增加 `demo` 协议（或 provider 类型），返回 OpenAI
  chat-completions 形状；流式按 chunk 发送。不访问网络。
- **Image**：`internal/mediagw` 的 provider 接口新增 demo 实现；资产 URL 指向
  `/v1/demo/assets/<name>.png`（服务端内置 PNG，按请求的 size/n 选择），并带
  `model: demo-image`。
- **Decision**：按 `kind=choice|predicate|score` 返回确定性结果（choice 固定选项、predicate
  固定概率、score 固定 levels）。
- **Realtime**：`internal/realtime` 增加 demo provider：连接后按 `response.create` /
  `input_audio_buffer.append` 节奏发出 `response.audio_transcript.delta/done` 与
  `response.done`；不依赖厂商，不需要音频设备。
- **Audio**：`POST /v1/media/audio/speech` 返回 WAV（demo：固定音高的短音）；`POST
  /v1/media/audio/transcriptions` 接收音频、返回固定转写。两者在未启用 demo 且无厂商时 503。

## 4. 客户端删除清单

| 位置 | 删除 |
|---|---|
| `surfaces/registry.ts` | `DEMO_PROVIDER_ID`、`DEMO_MODELS`、`isDemoSelection`、`previewSwitchReason` |
| `data/store.ts` | `surfacePreview` / `previewSurface` / `clearSurfacePreview` |
| `screens/model-picker.tsx` | demo 分支、面板演示分组、退出演示、正在预览、demo 确认文案 |
| `screens/conversation.tsx` | `demo()`、preview 解析分支、向 surface 传 `demo` |
| `surfaces/image/**` | demo prop、种子画廊、演示资产导入、演示控制 |
| `surfaces/realtime/**` | demo prop、演示注入抽屉、演示标记 |
| `surfaces/speech/**` | demo prop、演示转写、演示模式 |
| `assets/surfaces/**` | 演示图片（无引用后删除） |

## 5. 验收

1. 不设 env：`/v1/models` 无 demo 模型；选择器无演示分组；四个 surface 在无 provider 时都落
   配置态；全仓 `rg -i "演示|demo"` 在 `web/src` 只允许出现于测试的禁用断言与文档注释。
2. 设 env：选择器里出现 `demo` provider 的模型；LLM 能收到回复、图像能出图、realtime 能连上
   并收到转写、TTS 能播、STT 能转写；UI 不显示「演示」字样。
3. `nsh check`、`nsh test --target tui`、`go test ./...` 全绿。
