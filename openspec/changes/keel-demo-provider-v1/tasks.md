# Tasks: keel-demo-provider-v1

> 状态核对基线：2026-11-14，逐条按当前工作区代码取证（不是凭记忆）。取证落点写在各条后面。

## S1 客户端去 demo

- [x] 删除 registry 的 demo 模型表与 preview 通道（registry.ts 无 demo 符号）
      → `rg "DEMO_MODELS|DEMO_PROVIDER_ID|isDemoSelection|surfacePreview" web/src` 命中 0
- [x] store 删除 surfacePreview（store.ts 0 引用）
      → `rg "previewSurface|clearSurfacePreview" web/src` 命中 0
- [x] 选择器删除面板演示分组与演示分支（公网验证：选择器只剩真实 Provider 分组）
      → `rg -ni "演示|demo" web/src/screens/model-picker.tsx` 命中 0
- [x] conversation 不再向 surface 传 demo
      → `rg -ni "演示|demo" web/src/screens/conversation.tsx` 命中 0
- [x] image surface 删除演示种子/资产/控制（配置态 + 真实网关 7/7）
      → `web/src/surfaces/image/index.tsx` 只剩注释里的历史说明，无 seed/演示 prop；
        资产走 `generateImages`（`web/src/data/media.ts`）
- [x] realtime surface 删除演示注入与演示标记（realtime 12/12 + components 2/2）
      → `rg -ni "演示|demo" web/src/surfaces/realtime/index.tsx` 命中 0
- [x] speech surface 删除演示转写/演示模式（speech 8/8）
      → `rg -ni "演示|demo" web/src/surfaces/speech/index.tsx` 命中 0
- [x] 删除 `web/assets/surfaces/**`（目录已不存在）
      → `ls web/assets/surfaces` = No such file or directory；`rg "assets/surfaces" web/src web/*.js` 命中 0
- [x] `rg -i "演示|demo" web/src` 复核：非测试/非 fixture 命中 0
      → 剩余命中全部在 `*.test.tsx` 与 `web/src/data/fixture.ts`（开发期 scripted 服务，
        不是产品渲染分支），其余是解释性注释；`wideMode` 等是 grep 的 `idemo` 子串误命中

## S2 服务端 demo provider

并行拆分（每路一个子代理，各自独立验收）：
- S2a LLM：`internal/modelprovider`（已落地词汇层）+ `internal/provider` demo driver + modelcfg 注册
- S2b Image/Decision：`internal/mediagw` demo provider + `/v1/demo/assets/*`
- S2c Audio：`/v1/media/audio/speech` + `/v1/media/audio/transcriptions`
- S2d Realtime：`internal/realtime` demo WS provider + `/v1/realtime/status`

- [x] `KEEL_DEMO_PROVIDER` 开关与 demo 模型注册（llm/image/decision/realtime/tts/stt）
      → 开关词与身份：`internal/demo/demo.go`（`Env`、`ProviderID`、6 个 `Model*` 常量、
        `Enabled`/`EnabledFromGetenv`/`EnabledFromLookup`，只认 `1/true/yes/on`）；
        目录注册：`internal/modelcfg/demo.go`（`demoCatalogDocument()` 追加为 catalog layer，
        `DemoCatalog`/`DemoCatalogFromEnv` 是唯一读开关处）；
        装配点：`internal/servicehost/models.go:118` 用 `modelcfg.DemoCatalogFromEnv()`
- [x] modelprovider demo driver（非流式 + 流式）
      → 驱动实体在 `internal/provider/demo.go`（`DemoProvider.StreamCompletion` 走共享
        transport 解码器发 chat.completion.chunk；`Complete` 复用同一流拼非流式结果），
        词汇层/注册视图在 `internal/modelprovider/demo.go`；
        装配分支：`internal/servicehost/models.go:633` `case modelcfg.APITypeDemo → provider.NewDemoProvider()`；
        证据测试：`internal/provider/demo_test.go`（9 条，含流式/确定性/拒斥外来模型/取消）、
        `internal/modelprovider/demo_test.go`（2 条）、`internal/servicehost/demo_routing_test.go`（4 条）
- [x] mediagw demo image provider + `/v1/demo/assets/*`
      → `internal/mediagw/demo.go`（`demoGenerate`/`demoEdit`，按 size 选内置图块，
        n 张资产的 URL 用 `?i=<index>` 保持可寻址）+ `internal/httpapi/demo_assets.go`
        （`GET /v1/demo/assets/{name}`，开关关时 404，名字走闭集 `demo.AssetByName` 不走文件系统）；
        资产本体 `internal/demo/assets.go` + 3 个编译期 PNG；
        证据测试：`internal/mediagw/demo_test.go`（11 条）、`internal/httpapi/demo_assets_test.go`（7 条）
- [x] decision demo（choice/predicate/score）
      → `internal/mediagw/demo.go` 的 `demoDecision`：choice 固定选项、predicate 固定概率、
        score 取声明 levels 的中位；三种类型的形状与确定性由 `TestDemoDecisionAnswersShape`、
        `internal/httpapi/demo_assets_test.go:TestDemoDecisionAnswersAreDeterministic` 断言
- [x] realtime demo WS provider（S2d 后端已落地：`internal/realtime/demo.go` 的 `DemoProvider` + `/v1/realtime/status` 的 kind/models/采样率；选择顺序与测试见下。整体勾选留 S4）
      → `internal/realtime/demo.go`：`KindDemo` 走同一个 `RealtimeProvider`/`Proxy` 桥、
        同一套 `session.update` 注入；帧序列 `session.created` → `session.updated` →
        `response.created` → `response.audio_transcript.delta`×N → `...done` → `response.done`；
        `DefaultSessionUpdate` 声明 16k in / 24k out；
        选择顺序：`internal/realtime/provider.go` 的 `FromLookup`（5 条有序规则，与
        `docs/realtime-provider-setup.md` §2.2b 一致）；
        status：`internal/httpapi/realtime.go` 的 `realtimeStatusFromEnv`（demo 时报
        `provider/kind="demo"`、`models=["demo-realtime"]`，并从默认文档派生采样率）；
        证据测试：`internal/realtime/demo_test.go`（12 条，含 `TestFromLookupSelectionOrder`）、
        `internal/httpapi/realtime_demo_test.go`（5 条）
- [x] `/v1/media/audio/speech` 与 `/v1/media/audio/transcriptions`（demo 实现，未配置 503）
      → `internal/httpapi/media_audio.go`（路由注册 `registerMediaAudioRoutes`；
        speech 返回 16 kHz 单声道 16-bit WAV 且带 `x-keel-model`；transcriptions 收 multipart、
        上限 8 MiB，返回 `internal/demo` 的固定转写；开关关时两条都 503 且 body 不点名凭据/URL）；
        证据测试：`internal/httpapi/media_audio_test.go`（11 条）
- [x] capabilities 随 demo 开关报告 tts/stt/realtime
      → tts/stt：`internal/httpapi/media_audio.go` 的 `withAudioCapabilities`（只在 demo 开时
        置真，其余 flag 原样透传），`internal/httpapi/media.go:206` 是唯一调用点；
        realtime：不落在 `/v1/media/capabilities`（该 flag 在当前 token plan 下恒 false，
        `internal/mediagw/mediagw.go` 的 Capabilities 注释已写明），而是由
        `GET /v1/realtime/status` 的 `configured/kind/models/采样率` 报告——这也正是
        `web/src/surfaces/realtime/index.tsx` 走的 preflight 通道；
        证据测试：`TestAudioCapabilitiesFollowDemoSwitch`（demo on/off 两态）、
        `TestRealtimeStatusDemoProvider` / `TestRealtimeStatusDemoOff`

## S3 客户端真实接线

- [x] speech TTS：调用 `/v1/media/audio/speech`（synthesizeSpeech），未配置落配置态；播放走 AudioPlayer/Audio 平台能力
      → `web/src/surfaces/speech/index.tsx` 走 `synthesizeSpeech`（`web/src/data/media.ts:280`），
        503 → `MediaGatewayError("notConfigured")` → 面板配置态
- [~] speech STT：当前走平台 Speech 识别（真实麦克风→转写）；服务端 `/transcriptions` 客户端已就绪（transcribeAudio），待音频采集桥接后切换
      → 现状：`web/src/surfaces/speech/index.tsx` 的 STT 半边走 NSH 平台 `Speech.start`
        （`index.tsx:284`，zh-CN + interim），不经过服务端；
        就绪项：`transcribeAudio` 已在 `web/src/data/media.ts:315` 实现（multipart、503 分类、
        错误归一），但**当前无调用点**（`rg transcribeAudio web/src` 只命中定义处）——
        缺的是「麦克风音频采集 → Blob」这一段桥接；
        切换条件：拿到可上传的录音 Blob 后，把 STT 提交路径换成 `transcribeAudio` 即可，服务端无需再改
- [x] image / realtime 走既有真实路径（demo provider 由服务端充当）
      → image：`web/src/surfaces/image/index.tsx` 走 `generateImages` → `POST /v1/media/images/generate`；
        realtime：`web/src/surfaces/realtime/index.tsx` 走 `keel.realtime.*` 宿主桥 → `GET /v1/realtime`；
        两端都没有 demo 分支（S1 已删），demo 模型只是 `/v1/models` 里的普通选择

## S4 门禁

- [x] `nsh check` / `nsh test --target tui` 全绿
      → 2026-11-14 实跑：`nsh check` exit 0；`nsh test --target tui` **84 tests: 84 passed, 0 failed**
        （1m 57s；此前基线记的 82 条是同一 suite 的较早计数，现随新增用例增长）
- [x] `go test ./internal/... ./cmd/...` 全绿
      → 2026-11-14 实跑：全部包 `ok`（含 `internal/demo`、`internal/mediagw`、`internal/realtime`、
        `internal/modelcfg`、`internal/servicehost`、`internal/httpapi`）；`go build ./...` exit 0
- [x] 不设 env 的部署：界面无 demo 字样、无演示数据
      → 2026-11-14 实测（`acceptance.md` §条目1，证据 `evidence/`）：
        ① 源码 `web/src` 词表逐命中分类（非测试/非 fixture 命中 = 解释性注释 + `wideMode` 子串假阳性；
        `DEMO_*`/`previewSurface`/`assets/surfaces` 均 0 命中）；② 构建产物 `演示` 0 次、无 surfaces 演示资产残留；
        ③ 13760（进程 env 无 `KEEL_DEMO_PROVIDER`）浏览器快照：工作区与选择器全文无 `演示`/`demo`，
        Provider 分组只有真实厂商（`evidence/ui-noenv.txt` + `evidence/shots-noenv/*.png`）；
        ④ API：`/v1/models` 0 demo、`/v1/demo/assets/*` 404、`demo-image` 400、audio 两路由 503、
        `/v1/realtime/status` configured=false（`models-noenv.json`/`noenv-api-probes.txt`/`capabilities-noenv.json`/`demo-assets-noenv.txt`）
- [x] 设 env 的评审部署：四类 surface 端到端可用
      → 2026-11-14 实测（`acceptance.md` §条目2）：临时实例 `KEEL_DEMO_PROVIDER=1`（同 tenant/构建/凭据，
        端口 13791）四类 surface 逐字命令与输出——llm 会话链路（user.message→assistant.message→run.finished，
        文本逐字等于 `demo.ReplyText`，`provider=demo`）；image（`/v1/demo/assets/*?i=0|1` 200；
        同端点不带 model 仍走 vendor）；tts+stt（25222 B 16 kHz WAV 带 RIFF/ICMT 原句 → 转写读回原句+字节数；
        外来音频走固定兜底）；realtime WS（`session.created→session.updated→response.created→delta×6→
        transcript.done→response.done`）；另有浏览器复核（Demo 分组是普通 Provider，LLM 回复进界面）。
        **方法偏离**：same DB + `--no-worker` 实测不可行（13760 常驻 worker 抢走 run 并以
        `unknown provider "demo"` 终态失败，证据见 `acceptance.md` §2.0），故临时实例改用同 Postgres
        服务器上的独立库 `keel_verify_demo_v1`（收尾 drop）并保留唯一 worker
- [x] 报告：before/after 与 UNCOVERED 登记
      → `acceptance.md`（本次产出）：before/after 对照表（旧客户端演示面板 → 服务端 demo provider 的
        截图与事件流对照）、UNCOVERED 6 条（真实麦克风上行 / 客户端 STT 未切服务端 / 会话选择不持久化 /
        video 无 provider / decision 面未走浏览器 / 协议复跑用独立库的偏离说明）、进程与回收登记

## 并行推进纪律（Owner 2026-11-14「全速并行推进」）

四条子线各自持有独立文件集，禁止跨线编辑：
| 线 | 拥有文件 |
|---|---|
| S2a LLM | `internal/demo/**`、`internal/provider/demo*.go`、`internal/modelcfg/demo*.go`、`internal/servicehost/demo*_test.go` |
| S2b Image/Decision | `internal/mediagw/**`、`internal/httpapi/demo_assets*.go` |
| S2c Audio | `internal/httpapi/media_audio*.go` |
| S2d Realtime | `internal/realtime/**`、`internal/httpapi/realtime.go` |

冲突处置：同一文件只允许一个责任线写；发现他人文件正在编辑，等其落定后重跑门禁，不得抢改。
