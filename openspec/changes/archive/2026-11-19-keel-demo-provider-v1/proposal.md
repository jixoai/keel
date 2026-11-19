# Proposal: keel-demo-provider-v1

## Why

演示能力长在客户端里，客户端就无法上线：模型选择器里出现「面板演示」分组、surface 自带演示徽标/
演示注入/演示画廊种子，产品界面必须解释「这些不是真的」。Owner 裁决（2026-11-13）：

> 你不要在界面上搞 DEMO，而是要在服务上做 DEMO。你这样搞得我们这个客户端根本没法上线使用。

演示是**数据来源**问题，不是界面问题。客户端只应认识一件事：`/v1/models` 里有什么 provider、
这个 provider 能不能服务。演示应当是一个**服务端 provider**——它和真实厂商走同一套协议、同一套
surface、同一套错误与状态；生产部署不启用它，客户端里就什么都不出现。

## What Changes

### 1. 客户端零 demo 概念（删除）

- 删除 `DEMO_MODELS` / `DEMO_PROVIDER_ID` / `isDemoSelection` / `surfacePreview` 预览通道。
- 删除选择器「面板演示」分组、`演示` 徽标、`退出演示`、`正在预览`、跨 surface 的演示分支。
- 删除各 surface 的 `demo` prop、演示种子（图像画廊首轮）、演示注入（realtime）、演示转写
  （speech）、演示资产与 `assets/surfaces/**`。
- 客户端 MUST NOT 在 UI 里区分「真模型」与「演示模型」；它只知道 provider/model。

### 2. 服务端 demo provider（新增，默认关闭）

- 新增 provider 协议族 `demo`：从环境变量 `KEEL_DEMO_PROVIDER=1` 启用，注册为一组普通模型
  （llm / image / decision / realtime / tts / stt），身份是 `(providerId="demo", modelId=...)`。
- demo provider 与真实 provider 走**同一套**服务端路径：
  - LLM：`internal/modelprovider` 的 demo driver（OpenAI 兼容响应，支持流式）；
  - Image / Decision：`internal/mediagw` 的 demo provider（返回服务端托管的演示资产 URL；
    decision 返回确定性的 choice/predicate/score）；
  - Realtime：`internal/realtime` 的 demo WS provider（按协议发 `response.audio_transcript.delta`
    等事件，服务端生成，不依赖厂商）；
  - TTS/STT：`/v1/media/audio/speech` 与 `/v1/media/audio/transcriptions`（demo 返回合成音与
    确定转写）。
- 未启用时：`/v1/models` 里没有它们，各 surface 落配置态；客户端行为与生产一致。

### 3. 验收口径

- 生产部署（不设 `KEEL_DEMO_PROVIDER`）的界面上**不存在任何 demo 字样或演示数据**。
- 评审部署（设 `KEEL_DEMO_PROVIDER=1`）里，demo provider 在选型器里是一组普通 Provider 分组，
  每个 surface 由真实协议返回内容；界面文案不出现「演示」。
- 删除演示后 `nsh check` / TUI / Go 测试全绿；不再有 `assets/surfaces/**` 引用。

## Impact

- 客户端：`web/src/surfaces/**`、`web/src/screens/{model-picker,conversation,settings}.tsx`、
  `web/src/data/store.ts`；删除 `web/assets/surfaces/**`。
- 服务端：`internal/modelprovider`（demo driver）、`internal/mediagw`（demo provider + 音频端点）、
  `internal/realtime`（demo WS provider）、`internal/modelcfg`（demo catalog/env 开关）、
  `internal/httpapi/media.go`。
- 规格：本 change 的 `specs/demo-provider/spec.md`；`keel-session-surfaces-v1` 中关于「演示模式」
  的措辞由本 change 取代。
