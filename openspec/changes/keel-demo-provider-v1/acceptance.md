# Acceptance: keel-demo-provider-v1

收尾验收（2026-11-14）。对应 `tasks.md` 的三条未完成项：**不设 env 的部署**、**设 env 的评审部署
（四类 surface）**、**报告（before/after + UNCOVERED）**。

- 证据目录：`evidence/`（本次实测产物，随本文一并存档）；截图 `evidence/shots-noenv/`（无 demo 部署）、
  `evidence/shots-demo/`（demo 部署）。
- 环境纪律：口令一律经 `$KEEL_SERVE_PASSWORD` 从 gitignored `.env.local` 传入，**未出现在任何命令输出或本文件**。
- 临时进程/端口/库：见文末「§5 进程与回收」。

---

## 条目 1 —— 不设 env 的部署：界面无 demo 字样、无演示数据（勾）

### 1.1 源码词表审计（`web/src`，逐命中分类）

命令与原始输出：`evidence/grep-audit.txt`。

| 扫描 | 结果 |
|---|---|
| `rg -n -i "demo" web/src` | 86 行命中，落在 10 个文件（下表逐类说明） |
| `rg -n "演示" web/src` | **1 行**：`web/src/surfaces/speech/speech.test.tsx:57`（测试的禁用断言 `not.toContain("演示回复")`） |
| `rg -n "DEMO_MODELS\|DEMO_PROVIDER_ID\|isDemoSelection\|surfacePreview\|panel-demo" web/src` | **0 命中**（exit 1） |
| `rg -n "assets/surfaces" web/src web/*.js web/*.json` | **0 命中**（exit 1） |
| `ls web/assets/surfaces` | `No such file or directory` |
| 构建产物（13760 正在服务的 `web/.nsh/out/Keel/app.37dc0837d1.js`） | `演示` **0 次**；`demo-*` 命中的全部是 fixture 的 scripted 会话 id / 附件名（见下）；`find` 无 `surfaces/{aurora,prism,tidal,bloom}` 资产残留 |

86 行命中的分类（非测试、非 fixture 的位置**已清空**）：

| 文件 | 命中性质 | 说明 |
|---|---|---|
| `web/src/data/fixture.ts` | **开发期 scripted transport**（62 行） | 只在 `?fixture=1`（网页）或 `host.params.fixture===true`（TUI/测试）时启用（`store.ts:345-372`，唯一分支）；产品 URL 永不到达。`web/host.js` 全文 0 处 `fixture`；`/kit` 路由同样只在 dev 分支映射（`store.ts:60`）。属合理排除。 |
| `web/src/surfaces/image/image.test.tsx` | 测试数据 | 用字符串 `"demo-image"` 作**普通模型 id**（断言客户端不做任何 demo 区分）——正是本 change 要求的行为。 |
| `web/src/surfaces/speech/speech.test.tsx` | 测试数据/断言 | `demo-tts`/`demo-stt` 作为普通模型名；`演示回复` 是禁用断言。 |
| `web/src/screens/settings-capabilities.test.tsx` | 测试数据 | 错误文案样本含 `api_key=sk-…`（脱敏断言用）。 |
| `web/src/data/media.ts`（3 行） | 解释性注释 | 描述"不再有 demo 分支"的设计说明，无渲染逻辑。 |
| `web/src/data/store.ts`（3 行） | 解释性注释 | `store.ts:5/42/272` 说明 fixture 不是产品模式。 |
| `web/src/surfaces/image/index.tsx`（4 行） | 注释 2 行 + **子串误命中** | `:301` 注释提到 demo 路由；另 3 行是 `wideMode`（含子串 `deMo`）的假阳性。 |
| `web/src/surfaces/speech/App.tsx`、`components/transcript-row.tsx`、`index.tsx`（各 1 行） | 解释性注释 | 提及 harness-demo / demo sample / Demo STT 的历史说明。 |

结论：非测试/非 fixture 命中 = **解释性注释 + `wideMode` 子串假阳性**；无一处渲染分支或数据表。

### 1.2 真实部署（13760，KEEL_DEMO_PROVIDER 未设置）UI 快照

进程环境实证：`ps eww -p 60538` 的 `KEEL_*` 名单 = `API_KEY/BASE_URL/DSN/SERVE_ADDR/SERVE_PASSWORD/SERVE_TENANT`
——**无 `KEEL_DEMO_PROVIDER`**；其 worker 对 `demo/demo-llm` 的运行报
`providerId: unknown provider "demo"`（即无 demo catalog），反证开关关闭。
（走查后 16:43 该实例被并行工作线重启为 pid 35781：同一 tenant、同一 web 目录、**同一构建
`app.37dc0837d1.js`**、同样无 demo 环境——本节与 §1.3 的观测对象仍是同一部署形态。）

浏览器（headless Chrome + playwright-core，`evidence/ui-noenv.mjs`）实测，输出 `evidence/ui-noenv.txt`：

- 登录后工作区全文：`演示` **false**、`demo`（大小写不敏感）**false**；
- 打开既有会话 → 打开模型选择器：`演示` **false**、`Demo` 分组 **false**、`demo-` **false**；
- 选择器里的 Provider 分组只有真实厂商：`S2API / A6API / DeepSeek / Anthropic / OpenAI / OpenRouter / Ollama / Magpie`；
- 截图：`evidence/shots-noenv/01-workspace.png`（工作区，真实会话列表：42 行）、
  `evidence/shots-noenv/02-picker.png`（选择器，无演示分组/徽标）。

### 1.3 API 面（同一部署）无 demo 数据/能力

| 探针 | 结果 | 证据 |
|---|---|---|
| `GET /v1/models` | 200，10 个模型**全部**为真实 provider（s2api/a6api/deepseek/anthropic/openai/openrouter/ollama/magpie），**0 个 demo** | `evidence/models-noenv.json` |
| `GET /v1/demo/assets/{square,landscape,portrait}.png`（带会话） | **404** | `evidence/demo-assets-noenv.txt`；同请求在 demo 部署为 200（§2.3） |
| `POST /v1/media/images/generate {"model":"demo-image"}` | **400**（允许列表拒绝）`model "demo-image" is not allowed; use "bailian-token-plan/wan2.7-image"` | `evidence/noenv-api-probes.txt` |
| `POST /v1/media/audio/speech` | **503** `audio provider is not configured` | `evidence/noenv-api-probes.txt` |
| `POST /v1/media/audio/transcriptions` | **503**（同上） | `evidence/noenv-api-probes.txt` |
| `GET /v1/realtime/status` | `configured:false` | §条目2 对照 |
| `GET /v1/media/capabilities` | `tts:false stt:false realtime:false` | 同一次会话采样（`capabilities` 里 `realtime` 由 `/v1/realtime/status` 负责，见 spec 口径） |

**结论：勾。** 源码、构建产物、真实部署 UI 与 API 四个面均无 demo 字样/演示数据/演示能力。

---

## 条目 2 —— 设 env 的评审部署：四类 surface 端到端可用（勾）

### 2.0 方法与一处**方法偏离**（必须写明）

- 复用的既有浏览器证据：`../../../../../.agents/reports/keel/2026-11-13-session-surfaces/`（round3→round5，
  4396 行报告：before = 客户端演示面板；after = 服务端 demo provider 下六类 surface 浏览器走查）——
  本文件不重复消耗浏览器会话，改为在**新构建**上补协议级复跑 + 一轮浏览器复核。
- 临时实例（`/tmp/keel-app-demo`，本次新建）：
  `KEEL_DEMO_PROVIDER=1`、与 13760 同一 tenant UUID、同一构建（`web/.nsh/out`）、同一 vendor 凭据
  （`KEEL_BASE_URL`/`KEEL_API_KEY` 在位），监听 `127.0.0.1:13791`。
- **DB 选择偏离**：brief 建议 "same DB + `--no-worker`"。实测该组合**不可行**：same DB 上 13760 的常驻
  worker（无 demo 环境）会与临时实例争抢 run，且它对 demo 选择的执行是确定失败——
  实证：same DB 下 `demo/demo-llm` 的 run 被 13760 worker 抢到，3 次重试后终态
  `failed`，`lastError = agent: resolve model: modelcfg: demo-llm is not executable: providerId: unknown provider "demo"`。
  为让四类 surface **确定性**复跑且不向真库写入失败 run，临时实例改用**同一 PostgreSQL 服务器上的独立库**
  `keel_verify_demo_v1`（自建，收尾已 drop），并**保留 worker**（该库只有它一个 worker，无双 worker）。
  这与 brief 的意图（不新增第二个 worker 抢真库的任务）一致；对被测规格（demo provider 四类 surface 可用）无影响。
- 迁移与租户：`keel serve` 启动自动应用 schema；`POST /v1/tenants` 建同 UUID 租户（本实例设了
  `KEEL_ADMIN_KEY` 以便该调用通过 admin 门）。模型文档用部署文档（13760 租户文档的同一份 providers/selection）
  经 `--models-config` 注入，保证 "有 vendor 凭据的混合部署" 这一最难场景成立。

### 2.1 目录与能力面（开关开）

```
GET /v1/models → demo provider 6 模型（evidence/models-demo.json）
  demo/demo-llm       surface=llm      selectable=true
  demo/demo-image     surface=image    selectable=true
  demo/demo-decision  surface=generic  selectable=false   ← 只列出、不可作为会话模型（已裁决）
  demo/demo-realtime  surface=realtime selectable=true
  demo/demo-tts       surface=speech   selectable=true
  demo/demo-stt       surface=speech   selectable=true
GET /v1/media/capabilities → {"configured":true,"llm":true,"image":true,"decision":true,"tts":true,"stt":true,"realtime":false,...}
GET /v1/realtime/status    → {"configured":true,"provider":"demo","kind":"demo","models":["demo-realtime"],
                              "inputSampleRate":16000,"outputSampleRate":24000}
```

### 2.2 Surface ① LLM（会话链路）

```
curl -X POST /v1/sessions                          → 201 {"sessionId":"32fe…384c4"}
curl -X POST /v1/sessions/$SID/input -d '{"text":"你好，请用一句话自我介绍。",
     "selection":{"providerId":"demo","modelId":"demo-llm"}}'   → 201
GET /v1/sessions/$SID/run                          → status=done（约 1s 内）
GET /v1/sessions/$SID/events                       → 3 条：
  1:user.message | 2:assistant.message | 3:run.finished
  assistant.message.content[0].text = "已收到。这是 keel 服务端直接生成的回复：内容固定、可复现，
     不来自任何外部模型调用。你可以用它验证会话、流式增量与渲染链路。"
      （逐字等于 internal/demo.ReplyText）
  run.finished = {model:"demo-llm", provider:"demo", status:"done", turns:1, durationMs:22, stopReason:"end_turn"}
```

证据：`evidence/llm-events.json`。

### 2.3 Surface ② Image（生成端点）

```
POST /v1/media/images/generate {"model":"demo-image","prompt":"一只在窗边看雨的猫","size":"1024x1024","n":2}
→ 200，model=demo-image，assets=[{url:"/v1/demo/assets/square.png?i=0",width:32,height:32,model:"demo-image"},
                                 {url:"/v1/demo/assets/square.png?i=1",...}]
GET /v1/demo/assets/square.png?i=0 → 200 image/png 249B
GET /v1/demo/assets/square.png?i=1 → 200 image/png 249B
```

**路由对照（同一部署同一端点）**：不带 `model` 的请求仍走 vendor（返回签名 OSS URL，已脱敏只留分类）——
证明"显式 demo 走 demo、默认路径 vendor 优先"两条规则同时成立。证据：`evidence/image-generate.json`、
`evidence/image-default.json`。

### 2.4 Surface ③ TTS + STT（合成 → 转写往返）

```
POST /v1/media/audio/speech {"text":"明天下午三点在A会议室评审","model":"demo-tts"}
→ 200 audio/wav，25222 B，X-Keel-Model: demo-tts
  RIFF/WAVE PCM 16bit mono 16000Hz（dataBytes=25120）；RIFF INFO/ICMT chunk 在 25176 偏移携带原句
POST /v1/media/audio/transcriptions  (multipart file=同一条 25222 B WAV, model=demo-stt)
→ 200 {"text":"明天下午三点在A会议室评审（已接收 25222 字节音频）","model":"demo-stt"}
   ——文本来自音频自身携带的句子（真实往返），字节数如实计数
负控：同一端点上传非该音频（203 B 的 JSON 文件）
→ 200 {"text":"这是一段由服务端生成的固定转写文本：…（已接收 203 字节音频）","model":"demo-stt"}
   ——不冒充"听懂"了内容，也不回显上传内容
```

证据：`evidence/speech-headers.txt`、`evidence/stt-roundtrip.json`、`evidence/stt-foreign.json`。

### 2.5 Surface ④ Realtime（WS 事件序列）

客户端形态与 `web/host.js` 相同：`ws://…/v1/realtime?session_id=…&model=demo-realtime&provider_id=demo`，
onopen 发 `session.update`，收到 `session.updated` 后发 `response.create`。实测序列（`evidence/realtime-ws.txt`）：

```
21ms  session.created
22ms  session.updated
124ms response.created
125ms response.audio_transcript.delta ×6（"已收到。这是 keel " / … 逐字拼回）
125ms response.audio_transcript.done   transcript == 上述完整句子
125ms response.done
```

拼回的 transcript 与 `internal/demo.ReplyText` 逐字一致；`session.update` 走的是同一桥接（kind=demo）。

### 2.6 浏览器复核（同部署、新构建）

`evidence/ui-demo.mjs` → `evidence/ui-demo.txt` + `evidence/shots-demo/`：

- 选择器把 demo 作为**普通 Provider 分组**（"Demo"）：Demo LLM / Demo Image / Demo Realtime / Demo TTS / Demo STT
  五行，各自带能力事实（"图像生成 · 专用面板"/"实时语音 · 专用面板"/…）；无演示徽标、无"退出演示"。截图 `02-picker.png`。
- 选中 Demo LLM（芯片 → `Demo LLM · 高`，`03-picked.png`）后发送一条消息：界面出现完整回复
  "已收到。这是 keel 服务端直接生成的回复：…"（`04-llm-reply.png`），且整页无 `演示` 字样。
- 既有 round5 报告（同一 change 的浏览器走查）另有 Image/Realtime/TTS/STT 四面的 DOM 级证据与截图，
  路径见 `.agents/reports/keel/2026-11-13-session-surfaces/report.html`（步骤 12–16/18）。

**结论：勾。** 四类 surface（llm 会话链路 / image 生成端点 / tts+stt 往返 / realtime WS 事件序列）
在 `KEEL_DEMO_PROVIDER=1` 的真实部署上逐条走通；混合 vendor 凭据场景下显式 demo 路由与默认 vendor 路由各自成立。

---

## 条目 3 —— 报告：before/after 与 UNCOVERED 登记（勾）

本文即该报告。before/after 对照：

| 面 | Before（客户端内置演示） | After（服务端 demo provider，本 change） |
|---|---|---|
| 选择器 | "面板演示 · 不调用 Provider"分组、演示徽标（旧构建截图：`2026-11-13-session-surfaces/img/round3/picker-demo-section.png`） | 无演示分组；演示部署下是普通 "Demo" Provider 分组（`evidence/shots-demo/02-picker.png`；无 demo 部署下无 Demo 分组，`evidence/shots-noenv/02-picker.png`） |
| 图像面 | 顶部横幅「图像 Provider 尚未配置，当前为演示面板…本地演示资产」（`img/round3/image-demo.png`） | 服务端 demo 资产 URL（`/v1/demo/assets/*`，GET 200）或 vendor 真实输出；界面无演示文案（`img/round5/03-demo-image.png` + `evidence/image-generate.json`） |
| 语音面 | 演示徽标/演示转写 | TTS 返回 RIFF/ICMT 携带原句的真 WAV；STT 读回（`evidence/stt-roundtrip.json`） |
| 实时面 | 客户端演示注入 | 服务端 WS provider 真帧序列（`evidence/realtime-ws.txt`） |
| 生产安全性 | 客户端无法上线 | 不设 env 时 `/v1/models` 0 demo、demo 资产 404、能力 false、UI 无字样（条目 1） |

UNCOVERED / 限制（inherits findings-s4.md，逐条保留，无一条靠"看起来行"勾）：

1. **真实麦克风音频上行未覆盖**：浏览器无法自动授权麦克风；替代证据 = TTS→STT 的**真实 WAV 往返**（2.4）
   + round5 的"授权超时→降级文字"走查。真机麦克风链路留待有设备授权的走查。
2. **客户端 STT 仍走平台 Speech**（服务端 `/transcriptions` 已就绪、`transcribeAudio` 无调用点，缺"麦克风→Blob"桥接）。
   本轮验证的是**服务端端点**可用；客户端切换不在本 change 范围。
3. **会话模型选择不持久化**（内存态，刷新回落租户默认）——设计如此，建议下个 change 落库。
4. **video 无 provider**：视频面板占位（`surfaceKind=video` 不可达），单测覆盖、浏览器未覆盖。
5. **demo-decision 不可作为会话模型**（`selectable=false`，防进死胡同的裁决）；决策端点由既有 decision 测试覆盖，
   本轮未在浏览器走 decision 面。
6. **四类 surface 协议复跑在独立库 `keel_verify_demo_v1`**（同 Postgres 服务器、同 tenant、同构建）——
   原因见 §2.0；未在真库上制造第二 worker 竞争。

---

## §5 进程与回收（起过必回收）

| 进程/资源 | 端口 | 用途 | 回收 |
|---|---|---|---|
| `/tmp/keel-app-demo serve … --no-worker`（pid 14385，先起后用错） | 127.0.0.1:13791 | 首轮尝试（无 worker，被 13760 worker 抢跑，失败原因即 §2.0 的实证） | `kill 14385`，`lsof -nP -iTCP:13791 -sTCP:LISTEN` 0 行 |
| `/tmp/keel-app-demo serve …`（pid 19953/21950，最终实例） | 127.0.0.1:13791 | 四类 surface 复跑 + 浏览器复核 | 见文末收尾记录 |
| PostgreSQL 库 `keel_verify_demo_v1` | — | 临时实例的隔离队列 | 收尾 drop（见文末） |
| headless Chrome（playwright 自起自停） | — | 两次 UI 快照 | 脚本 `browser.close()`，无残留监听 |
| 13760 常驻实例 | 13760 | 不设 env 部署审计（只读） | **未动**（无重启、无 drain） |

> 收尾后的复核输出（`lsof` 与 drop 结果）补记于下，字号与 §2.0 的偏离说明共同构成"不污染真库/真服务"的证据。

### 收尾记录（2026-11-14，实测输出）

```
$ lsof -nP -iTCP:13791 -sTCP:LISTEN        # 起实例时
keel-app- 21950  kzf  15u  IPv4 … TCP 127.0.0.1:13791 (LISTEN)
$ kill 21950; sleep 2
$ lsof -nP -iTCP:13791 -sTCP:LISTEN | wc -l
0
$ ps -p 21950                                 # → process gone
$ go run ./tmp-verify-db drop keel_verify_demo_v1
drop keel_verify_demo_v1: ok
$ go run ./tmp-verify-db list | grep -c verify_demo
0
$ rm -rf tmp-verify-db                        # 一次性建库工具已删除，无仓内残留
```

- 13760 全程未动（无重启、无 drain、无写操作；只做了带认证的读请求与浏览器只读走查）。
- 另：审计用的构建身份已在 §1.1 记录（`app.37dc0837d1.js`，mtime 2026-11-14 13:56，13760 实际服务的那一代）；
  被审计的 demo 相关源文件（`surfaces/{image,speech}`、`model-picker.tsx`、`data/{store,fixture}.ts`）自 13:56 起无改动。
