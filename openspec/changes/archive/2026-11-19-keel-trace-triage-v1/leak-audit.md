# T2 泄露审计 — keel-trace-triage-v1

**结论：未发现真实泄露。**

Trace 全量扫描（文本 + 全部 JSON 字符串字段）与客户端/服务端代码路径交叉检查后，
**没有任何一处凭据形态、会话凭据、个人数据或内网地址出现在 trace 中**，也没有找到
「产品代码把凭据写进日志 / DOM / URL / 存储」的可达路径。

本文件只记录**类型、位置、是否含真实值**，不含任何原文值。

---

## 0. 审计对象与方法

| 项 | 值 |
|---|---|
| 输入 | `Trace-20261113T225129.json.gz`（Chrome DevTools TraceEvents） |
| SHA-256 | `a60770bb232b4b9c23aea2c976117ccfbf98cb79f111b628cfb4b6a7b845e5dc` |
| 解压后大小 | 14,483,185 字节 |
| 事件数 | 15,123 |
| 字符串叶子节点 | 208,026 |
| 解压落位 | `/tmp/keel-trace-leak/`（隔离目录，非仓库内） |
| 脚本落位 | `/tmp/keel-audit-scripts/`（与数据分离） |
| 解释器 | 全部 `python3 -I`（隔离模式，不加载 cwd/脚本目录的同名模块） |

**采集口径（关键）**：该文件 `metadata.source = "DevTools"`、`dataOrigin = "TraceEvents"`，
是一份**性能面板 trace**，不是 Network 面板导出。这决定了它的证据面：

- **不含** `ResourceSendRequest` / `ResourceReceiveResponse` / `ResourceFinish`（HTTP 请求头/请求体/响应体）
- **不含** `ConsoleMessage` / `LogEntry`（控制台消息）
- **不含** `Network.*` / `Fetch` / `XHR` / `WebSocket`（网络层事件）
- **不含** `UserTiming` / `MarkLoad` / `MarkDOMContent`（导航用户计时）
- **含** 的类别：`loading`、`devtools.timeline`、`v8`、`v8.execute`、`cppgc`、
  `disabled-by-default-v8.cpu_profiler`、`disabled-by-default-devtools.screenshot`、
  `disabled-by-default-devtools.v8-source-rundown(-sources)`、`blink.*`、`cc.*`

因此「请求头/请求体/响应体/console 消息」这四类表面在本 trace 中**结构上不存在**。
审计按「实际存在的表面逐项证明为空 + 不存在的表面显式登记为不可判」执行（见 §3.4）。

---

## 1. 命中表

**命中总数为 0。** 下表列出全部被扫描的形态，以及其在 trace 中的实测计数：

| 编号 | 类型 | 位置（trace 表面） | 含真实值? | 代码位置 | 风险 |
|---|---|---|---|---|---|
| — | `Authorization` 形态 | 全文（键名与值） | **不含**（0 次） | 见 §2 | 无 |
| — | `Bearer ` 形态 | 全文 | **不含**（0 次） | 见 §2 | 无 |
| — | `sk-` 前缀密钥 | 全文 | **不含**（0 次） | 见 §2 | 无 |
| — | `api[_-]?key` / `apiKey` | 全文 | **不含**（0 次） | 见 §2 | 无 |
| — | `token=` 查询参数 | 全文 + 全部 URL 查询串 | **不含**（0 次） | 见 §2 | 无 |
| — | `password` / `Password` | 全文 | **不含**（0 次） | 见 §2 | 无 |
| — | `cookie` / `Cookie` / `set-cookie` | 全文 | **不含**（0 次） | 见 §2 | 无 |
| — | `session` / `Session` | 全文 | **不含**（0 次） | 见 §2 | 无 |
| — | 邮箱 | 全文 | **不含**（0 次） | — | 无 |
| — | 中国手机号（11 位） | 全文 | **不含**（0 次） | — | 无 |
| — | 绝对路径 `/Users/<name>` | 全文 | **不含**（0 次） | — | 无 |
| — | `127.0.0.1` / `localhost` | 全文 | **不含**（0 次） | — | 无 |
| — | 内网地址 `192.168.*` / `10.*` | 全文 | **不含**（0 次） | — | 无 |
| — | 模型供应商域名 | 全文 | **不含**（0 次） | — | 无 |
| — | JWT（`eyJ…`三段） | 全文 | **不含**（0 次） | — | 无 |
| — | 长十六进制串（≥32，大小写不敏感） | 全文 | **不含真实凭据**（1,511 次命中，但仅 **2 个不同值**，全部为 Chromium 渲染帧 token，见 §1.4） | — | 无 |

> 上表「0 次」为两种独立方法的一致结论：`python3 -I` 正则全量扫描 + `LC_ALL=C grep -a -o -i`
> 逐形态字节级复核。正对照（`keel`/`fde2e`/`traceEvents`/`nsh-v`/`ScriptCatchup`/`frame_reporter`）
> 均返回非零计数，证明扫描管道确实读到了文件内容，阴性结果不是「读不到文件」造成的假阴性。

### 1.1 唯一存在的 URL 表面（全部为同源静态资源与客户端路由）

trace 中共 15 个不同 https URL，**全部**属于部署主机 `local-gaubee-keel-app.fde2e.com`，
**无一个带查询串**（`?` 计数为 0，因此不存在 `?token=…` / `?api_key=…` 类路径）：

| 类别 | 形态 | 含真实值? | 备注 |
|---|---|---|---|
| 应用入口 | `/` | 不含 | 根路径 |
| 客户端路由（3 个） | `/<uuid>` | 不含 | 会话 id（**非凭据**，见 §3 备注） |
| 构建产物 | `/app.<hash>.js`、`/host.<hash>.js`、`/nsh.<hash>.js`、`/nsh.<hash>.wasm` | 不含 | 内容哈希，非密钥 |
| 运行时 | `/browser.<hash>/{bootstrap,loader,realm,runtime}.js` | 不含 | 内容哈希 |
| 静态素材 | `/assets/assets/surfaces/image/{aurora,prism,tidal}.<hash>.png` | 不含 | 内容哈希 |

`blob:` 伪 URL 3 个（`blob:https://…/<uuid>`），为应用内存对象，无凭据。

### 1.2 第三方扩展（非本产品代码）

trace 中含 **5 个不同 Chrome 扩展 ID、共 23 个扩展脚本 URL**（`chrome-extension://<id>/…`）。这些是浏览器侧
装的其他扩展，属于**审计环境**而非产品代码路径；它们只出现在 `v8-source-rundown` 的
script 清单里，**源码体已被 stub**（`StubScriptCatchup` 只带 `isolate`/`scriptId`，
`data` 中 `function`/`fetch`/`localStorage`/`Authorization`/`token`/`key` 计数均为 0）。
即：本 trace **不包含任何 JS 源码正文**，因此不存在「源码里硬编码密钥」这一类表面。

### 1.3 截图表面

5 张 `Screenshot` 事件，base64 JPEG，500×458，**5 张 MD5 完全相同**
（`699bf6d7305f38ae4bcaca643e2dd58e`）——同一帧，非 5 个不同时刻。
目视核对：画面为 Keel 应用 UI（左侧会话列表 + 右侧「生成表面 / 负面提示词 / 比例 / 清晰度 /
数量 / 种子」表单），**无可视凭据、无密钥文本、无邮箱/手机号**。

### 1.4 唯一「长十六进制串」表面（判定为非凭据）

`[0-9A-Fa-f]{32,}` 大小写不敏感扫描共命中 **1,511 次**，但**只有 2 个不同取值**，
且**全部为大写、定长 32 字符**，分布如下：

| 键路径 | 命中次数 | 判定 |
|---|---|---|
| `args.data.frame` | 1,485 | Chromium `cc` 渲染帧 token（content-layer frame），非凭据 |
| `args.data.page` | 10 | 页面级 frame token（同 1 个取值），非凭据 |
| `args.beginData.frame` | 8 | 同上 |
| `args.frame` | 5 | 同上 |
| `args.data.frames[].frame` / `[].parent` | 2 / 1 | 同上 |

判定依据：① 只有 2 个取值而出现 1,511 次——真实密钥不会在高频事件里复用同一值；
② 全部是 32 位大写十六进制，形态为 Chromium content-layer frame token（与
`frame_reporter`/`beginData.frame`/`layoutRoots` 等渲染事件同簇出现）；
③ 与 15 个 URL、任何请求/凭据表面零关联。依「不落原文」纪律，本报告不复制其值。

> 修正记录：本报告初稿在「长十六进制串」一行曾写 0 次，原因是首轮正则为小写
> `[a-f0-9]`，而 Chromium 输出大写。已改用大小写不敏感正则重扫并在此如实登记
> 命中数与判定。其余凭据形态的 0 命中经大小写不敏感复核后仍为 0。

---

## 2. 代码交叉检查（是否有「写出去」的路径）

### 2.1 客户端：`web/src/**` 与 `web/host.js`

| 检查项 | 结论 | 证据 |
|---|---|---|
| `console.log/error/warn/…` 打印请求头 | **无** | `web/src` 非测试代码中 `console.*` 仅 **1 处**：`web/src/data/fixture.ts:1158` — `console.warn("fixture script failed", error)`，只打印 fixture 脚本异常对象，不涉及请求头/凭据；`web/host.js` 中 `console.*` **0 处** |
| URL 带 query 凭据 | **无** | `web/src` 全量 `URLSearchParams` 仅 3 处：`web/src/data/store.ts:317`（`fixture=1` 开发开关）、`web/src/data/http.ts:357`（`limit`）、`web/src/data/http.ts:669`（`after` 游标）。均为非敏感分页/开关参数 |
| 把 provider key 放进 DOM / URL | **无** | 凭据字段在 `web/src/screens/settings.tsx:876` 只绑定**引用**（`S2API_KEY` / `secret:name`），hint 明写「这里永远不存凭据的值」；密钥值输入框 `web/src/screens/settings.tsx:1389` 为 `masked`，值只存在本地 signal，保存后 `secretValue.set("")`（`:1324`）清空 |
| 错误消息回显 Authorization | **无（且主动防御）** | `web/src/data/http.ts:130-140` 的 `isSafeReason()` 在**任何 screen 看到之前**丢弃含 `api_key/token/secret/bearer/authorization/password` 的文本、含 `https?://` 的文本、含 Go 栈帧/传输错误的文本、含 ≥32 位疑似 token 的文本 |
| 401 原文上屏 | **无** | `web/src/data/store.ts:386-388`：401 → 固定文案「口令不正确」；其它才用 `messageOf` |
| 凭据进 localStorage / sessionStorage / cookie | **无** | `web/src` + `web/host.js` 中 `localStorage`/`sessionStorage`/`indexedDB`/`document.cookie` 实测 **0 处写入**（唯一出现是 `web/src/ui/kit/layout/settings-shell.tsx:15` 的注释文字，非调用）；构建产物 `web/dist/.../app.*.js` 中 `localStorage|sessionStorage` 计数为 **0** |
| 请求带凭据的方式 | **安全** | `web/src/data/http.ts:303,323,677` 一律 `credentials: "same-origin"`；凭据只由服务端下发的 **HttpOnly** cookie 承载（`cmd/keel/auth.go:243-251`），JS 读不到 |
| WebSocket URL 夹带 token | **无** | `web/host.js:334-343` 的 `wsUrl()` 只拼 `session_id` / `model` / `provider_id`，**无 token/密钥** |

### 2.2 服务端：`internal/httpapi/**` 及相邻层

| 检查项 | 结论 | 证据 |
|---|---|---|
| 错误响应回显请求头 | **无** | `internal/httpapi/*.go` 中 `r.Header.Get` 只做**读取校验**，读取值从不进入响应体；`Authorization`/`X-Admin-Key` 的比较结果只产出固定文案（`internal/httpapi/admin.go:50-63`：`"admin bearer token required"` / `"invalid admin token"`；`internal/httpapi/mcp_oauth.go:230-240` 同理只返回 bool） |
| 请求日志记录头/查询/体 | **无（显式设计）** | `internal/httpapi/logging.go:1-3` 开宗明义「Headers, query strings, and bodies are never logged」；实测 `serveLogged` 只记录 `request_id/method/route/status/bytes/duration_ms` + 合法 UUID 的 `tenant_id`/`session_id`（`logging.go:88-121`） |
| 日志中凭据值脱敏 | **有防护** | `internal/logsafe/logsafe.go` 双层脱敏：①按 key（`authorization`/`api key`/`token`/`secret`/`password`/`body` 等整体替换）；②按值（`Bearer <token>`、JSON `"api_key":"…"` 等就地掩码）。`internal/logsafe/logsafe_test.go:13-50` 断言四类凭据均不出现在输出中，且断言 `authorization` 字段等于 `[REDACTED]` |
| `writeErr` 是否原样回显 `err.Error()` | **是（泛化机制，见 §2.3）** | `internal/httpapi/server.go:1085-1087`、`internal/agentapi/api.go:260-262`：`writeJSON(w, status, {"error": err.Error()})` |
| 网关错误是否夹带响应体/URL | **有界且分类** | `internal/gateway/http.go:223` 用 `readBoundedExcerpt`（截断到 `errorMessageBodyChars`）取上游响应片段；`internal/gateway/errors.go:167-177` 明确「Body is a bounded excerpt, never the whole body」。上游 4xx/5xx 片段理论上可能含上游自己的报错文本，但**不含本服务的凭据**（本服务从不把它自己的 Authorization 写进提示） |
| OAuth 回调页回显 | **有界且转义** | `internal/httpapi/mcp_oauth.go:468-481` 的 `writeOAuthPage` 对 `title`/`details` 做 `html.EscapeString`；`details` 来源是 `err.Error()`，但 OAuth 库在构造错误前已用 `redactSecrets(...)` + `authorizationSecret(...)` 剔除 `client_secret_basic` 的 base64（`internal/mcp/oauth/flow.go:340-357, 416-429`） |
| 密钥列表接口泄露值 | **无（契约级）** | `internal/agentapi/settings.go:489-495`：`secretsResponse` 只含 `name`/`createdAt`/`updatedAt`，注释明写「never carries a value, a length or any other datum about one」；`putSecret`（`:545` 起）注释「never echoed, never logged and never part of an error」 |
| 实时语音连接失败回显 provider URL | **无（显式设计）** | `internal/httpapi/realtime.go:203-209`：provider 不可用时 body **不带 reason**，理由明写「the reason can name the provider URL. Operators read the detailed diagnostic from the logs」；`realtime.go:146` 上屏固定文案 `"realtime provider is unavailable"` |
| 实时 bridge 日志脱敏 peer 文本 | **有防护** | `internal/httpapi/realtime.go:311-330` 的 `realtimeLogError`：close error 只留 code（丢弃 peer 可控的 reason）、其余截断 200 字符、`reason = ` 段替换为 `[redacted]` |
| 请求 URI 原样回显 | **仅 1 处，非响应体** | `internal/mcp/oauth/callback.go:213-214` 重建 `requestURL` 但**只用于路径白名单比对**，不写入响应 |
| 请求 Dump / `httputil.DumpRequest` | **无** | 全仓 `DumpRequest`/`httputil` 计数为 0 |
| 凭据可持久化进 provider 文档 | **被拒绝（防御到位）** | `internal/modelcfg/catalog.go:643-659` 的 `validateSecretRef`：只接受 `${NAME}`/`env:NAME`/`secret:NAME`，明文密钥报错；`:684-699` 的 `validateHeaders` 拒收 `authorization`/`x-api-key` 等携带凭据的 header；`:622-638` 的 `validateBaseURL` 拒绝 URL 内嵌 `user:pass` |

### 2.3 唯一「泛化回显」机制的收敛判定

`writeErr` 把 `err.Error()` 原样写进响应体，是一个**泛化面**：如果某个 err 在构造时拼进了
敏感值，就会经由它上屏/进浏览器。逐条核查所有 `err.Error()` 的构造源后，结论为**当前无实害**：

1. **`access.env` 自回显**（`internal/modelcfg/catalog.go:494`）——`fmt.Errorf("access.env %q must be …",
   provider.Access.Env)` 会把**用户填的 access.env 值**回显。但该函数仅由 `LoadCatalog` 调用
   （`internal/modelcfg/catalog.go:152`），而 `LoadCatalog` 的唯一调用点是
   `EmbeddedBuiltin()`（`internal/modelcfg/catalog.go:72`，加载**编译期内嵌**的 catalog，
   失败即 panic）。**用户请求无法触达**，因此不构成泄露面。风险等级：低（若将来给该
   validate 加用户入口，会自动升级为「用户输入自回显」）。
2. **`credentialRef` 自回显**（`internal/modelprovider/validate.go:983-991`）——非引用形态时用 `%q`
   回显 `ref`。这就是用户输入自己的**凭据引用**（应为 `secret:name` 形态）；当用户误填了明文
   密钥时，这段明文会回到**填它的同一个已认证用户**的响应里。属「用户输入自回显」，不跨用户、
   不跨权限边界，但**原则上是应当掩码的形态**（见 §4 R1）。
3. `internal/httpapi/agentteam_dto.go:430-491` 的 `ModelProviderWriteRequest`（会走上面第 2 条）
   目前**无调用点**（全仓仅定义处出现），属未接线代码——当前不构成可达路径。

---

## 3. 阴性证据：扫描过的形态与位置（全量清单）

### 3.1 扫描过的形态

凭据形态：`Authorization`、`Bearer `、`sk-<token>`、`api[_-]?key`、`apiKey`、`API_KEY`、
`token=`、`password`/`Password`、`cookie`/`Cookie`/`set-cookie`、`session`/`Session`、
JWT（`eyJ` 三段式）、≥32 位长十六进制串、`*_KEY`/`*_TOKEN`/`*_SECRET`/`*_PASSWORD` 环境变量名形态。

个人数据形态：邮箱（RFC 形）、中国手机号（`1[3-9]\d{9}`）、绝对路径 `/Users/<name>`、
回环 `127.0.0.1`、`localhost`、内网 `192.168.*` / `10.*`。

基础设施形态：模型供应商域名（`openai.com`/`anthropic.com`/`api.deepseek`/`dashscope`/`aliyuncs`/
`azure.com`/`generativelanguage`/`googleapis`/`openrouter`/`moonshot`/`bigmodel`/`volces`/
`siliconflow`/`groq.com`/`mistral`/`cohere`）、任意 `https?://` URL、任意 `key=value` 形态串。

### 3.2 扫描过的位置（JSON 字段级，非仅文本）

遍历 `traceEvents[*]` 全部 **208,026** 个字符串叶子，逐个键路径核对，覆盖：

| 键路径 | 叶子数 | 审计结论 |
|---|---|---|
| `args.data.cpuProfile.nodes[].callFrame.url` | 43,925 | 仅 6 个不同值，全部同源静态资源 + 1 个扩展脚本；无查询串、无凭据 |
| `args.data.cpuProfile.nodes[].callFrame.functionName` | 46,331 | 混淆名（`$funcN`）与内建名，无凭据形态 |
| `args.data.cpuProfile.nodes[].callFrame.codeType` | 46,331 | 枚举值 |
| `ev[].cat` / `ev[].name` / `ev[].ph` | 各 15,123 | 事件分类/名称/相位，无凭据 |
| `args.context.URL` | 4,708 | **全部空串**（`''`） |
| `args.nodeDebugName` / `args.data.nodeName` / `args.endData.layoutRoots[].nodeName` | 4,704 / 452 / 4 | 仅 class 名（`DIV class='nsh-v'` 等），无文本内容、无用户数据 |
| `args.data.url` / `args.data.stackTrace[].url` / `args.animation_frame_script_timing_info.source_location_url` | 956 / 858 / 9 | 同上 6 个同源 URL 的子集 |
| `args.data.image_url` / `ukm_page_load_timing_update.latest_url` / `args.data.frames[].url` | 12 / 3 / 2 | 同源根路径与 3 个静态素材，`about:blank` ×1 |
| `args.snapshot` | 5（各 ~31 KB） | base64 JPEG（截图），非文本；目视已核（§1.3） |
| `args.data.isolate` / `scriptId` / `functionName` | 994 / 903 / 903 | 引擎内部标识，无凭据 |
| `args.data.frame` / `args.beginData.frame` / `args.frame` / `args.data.page` / `args.data.frames[].*` | 1,511（合计） | 32 位十六进制**渲染帧 token**（仅 2 个取值），非凭据（见 §1.4） |
| `args.data.object_name` / `type` / `source` / `nodeName` | 48 / 171 / 643 / 452 | 引擎枚举与 DOM 描述 |
| `args.taskName` / `args.worldName` / `args.page` / `args.uptime` | 18 / 6 / 10 / 3 | 运行时元数据 |
| `args.max_duration_ms` / `args.value.*` / `args.tileData.*` / `args.chrome_latency_info.*` | 21 / 40 / 24 / 9 | 数值/枚举 |
| `args.args.name` / `args.id` | 34 / 14 | 内部 id |
| `id2.local` / `id` | 5,083 / 646 | 事件 id |

### 3.3 一处「看起来像凭据但判定为非凭据」的说明

1. **32 位十六进制渲染帧 token**（详见 §1.4，1,511 次命中、仅 2 个取值）——
   判定为非凭据，理由见 §1.4。
2. **会话 UUID**（3 个客户端路由 `/<uuid>`、3 个 `blob:…/<uuid>`）——客户端会话标识，**不是凭据**：
   它不授予任何权限（鉴权由 HttpOnly cookie 承载，见 §2.1），且本 trace 中它只出现在
   客户端路由与 blob URL 中，未与服务端凭据配对。

### 3.4 明确「不可判」的表面（本 trace 结构上不含）

下列表面在本 trace 中**不存在相应事件类型**，因此**既不能证有也不能证无**；本次审计
以「代码路径核查」替代（§2），结论为代码侧无泄露路径：

- 请求头（`Authorization`/`Cookie` 等）——需 Network 面板导出
- 请求体 / 响应体——同上
- 控制台消息（`console.*` 输出）——需勾选 Console 或含 `ConsoleMessage` 类别
- localStorage / sessionStorage 快照——需 Application 面板或专用 trace 类别

---

## 4. 修复建议（按泄漏面）

**当前无真实泄露，因此没有「必须立即修」的项。** 以下为按泄漏面排序的加固建议，
前两条是把 §2.3 的泛化面收窄，属「纵深防御」而非事故修复。

| 优先级 | 泄漏面 | 建议 | 落点 |
|---|---|---|---|
| **R1（建议修）** | 用户输入自回显：凭据引用 | 把非引用形态的错误消息从 `%q` 回显改为不回显值，只描述期望形态（与 `isSafeReason` 的既有立场一致：错误里不出现疑似密钥）。同步补一条测试断言「错误文本不含输入值」 | `internal/modelprovider/validate.go:983-991` |
| **R2（建议修）** | 用户输入自回显：`access.env` | 同 R1 处理 `%q`；即便当前不可达（仅内置 catalog），也应消除「将来接用户入口即成洞」的形态 | `internal/modelcfg/catalog.go:494` |
| **R3（可选）** | `writeErr` 泛化回显 | 在 `writeErr` 出口加一层兜底脱敏（复用 `logsafe` 的值级掩码），使任何将来新增的错误构造都无法把 `Bearer …`/`sk-…`/JSON `api_key` 送上屏 | `internal/httpapi/server.go:1085`、`internal/agentapi/api.go:260` |
| **R4（保持）** | 网关上游响应片段 | 现状（有界截断 + 只取片段）足够；建议补测试固定「上游 401 片段里的凭据不出现」，防止将来放宽 | `internal/gateway/http.go:223, 628-635` |
| **R5（保持）** | 请求日志 | 保持 `logging.go` 的「头/查询/体一律不记」不变式，建议加一条测试锁死（当前靠代码注释与实现） | `internal/httpapi/logging.go:88-121` |
| **R6（流程）** | trace 采集口径 | 下次取证请在 DevTools 勾选 **Console + Network** 后再录制，否则「请求头/请求体/console」三类表面永远不可判（§3.4）。若需排查凭据是否进请求，这是唯一路径 | 取证 SOP |

---

## 5. 可直接引用的阴性结论

- trace 中 `Authorization`、`Bearer`、`sk-`、`api_key`、`apiKey`、`token=`、`password`、
  `cookie`、`set-cookie`、`session` 全部 **0 次命中**（大小写不敏感正则 + 字节级 grep
  双方法一致，正对照非零）。
- trace 中「带查询串的 URL」**0 个**，因此不存在 URL 夹带凭据。
- trace 中含 **1,511 次**长十六进制串，但**仅 2 个不同取值、全部为 Chromium 渲染帧
  token**（详见 §1.4），非凭据。
- trace 中**不含任何 JS 源码正文**（source-rundown 已 stub），因此不存在「源码硬编码密钥」表面。
- trace 中**不含** HTTP 请求/响应事件与 console 事件（性能 trace 的采集口径决定，见 §0）。
- 客户端**不向** localStorage/sessionStorage/indexedDB/cookie 写任何值；构建产物同结论。
- 客户端唯一 1 处 `console.warn` 只打印 fixture 异常对象，不涉及请求头/凭据。
- 服务端请求日志**不记**头/查询/体；`logsafe` 双层脱敏且有测试断言。
- 服务端**不**在任何错误响应里回显请求头；密钥列表接口契约级不返回值。
