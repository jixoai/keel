# MODEL-CATALOG — 模型目录：凭据、发现、逐模型事实与价格

> 本文是 provider/model 配置层的单源说明（change `keel-model-catalog`）。
> 字段与代码的唯一真值仍是 `internal/modelcfg`（解析/校验）与
> `web/src/data/model-catalog.ts`（界面派生）；本文只解释词汇、默认值、
> 边界与取舍，不复制实现。
>
> 产品参照：Pi（`docs.pi.dev/latest/models-configuration`）、Magpie
> （yetone/magpie，逐模型上下文/输出与预设、分层与闲时价格、免费标记、
> provider 编辑器）、ZCode（zai-org/ZCode，OpenAI/Anthropic provider 接入、
> 大上下文与 input/cached/output 价格）。三者只作**产品参照**，不是可从之
> 直接抄的代码约定。

## 1. 字段词汇

模型配置的文档层是 `modelcfg` 的稀疏补丁（JSON Merge Patch：缺省继承、
`null` 清除、具体值替换、数组整体替换）。层级固定为
`modelRules → modelApiRules → siteRules → providerModelRules`，同层内后者
覆盖前者；`providerModelRules` 以精确 `(providerId, modelId)` 命中，是"逐模型"
编辑（界面「参数」对话框）写入的那一层。

| 字段 | 类型 | 含义 | 默认 / 未声明 | 空值语义 |
|---|---|---|---|---|
| `enabled` | bool | 该模型是否可执行/可选 | `true` | `false` 保留并给出 issue |
| `providerType` | token | 模型家族（覆盖 provider 家族） | 继承 provider（缺省 `llm`） | 只能缺省，不能 `""` |
| `properties.contextWindow` | int | 上下文 / 最大输入（tokens） | 无 → 不完整（`>0` 才可执行） | `0` 视为不完整 |
| `properties.inputFormat.{text,image,video,audio,pdf}` | bool | 可接受的输入模态 | 全 `false` | 逐键布尔 |
| `properties.outputFormat.{text,image,video,audio}` | bool | 产出模态（无 `pdf`） | 全 `false`（llm/realtime 必须 `text`） | 逐键布尔 |
| `properties.supportsToolCall` | bool | 工具调用（运行时工具门已强制） | `false` | — |
| `properties.supportsJsonSchemaOutput` | bool | JSON Schema 输出 | `false` | — |
| `properties.supportsNativeWebSearch` | bool | 原生联网搜索 | `false` | — |
| `options.reasoningLevel` | `{values[], map}` | 思考档位与请求补丁 | 无 → 不完整 | — |
| `options.maxOutputTokens` | `{max, map}` | 单次输出上限 | 无 → 不完整 | — |
| `pricing.currency` | ISO-4217（3 位大写） | 币种 | 无 → 界面只显示数字 | 不发明符号 |
| `pricing.input` / `output` / `cacheRead` / `cacheWrite` | number ≥0 | 每 **1M tokens** 的价格 | `undefined` = 未声明 | `0` 是真实价格，不是未声明 |
| `pricing.free` | bool | 免费/零价标记 | `false` | 只用于展示与徽章 |
| `pricing.notes` | string ≤200 字符、无换行 | 备注（闲时折扣、来源） | 无 | 只用于展示 |

`pricing` 与 Pi 的 `cost{input,output,cacheRead,cacheWrite}`、Magpie 的
tiered/off-peak 价格、ZCode 的 input/cached/output 三档对位：四档是
input/output/cacheRead/cacheWrite，单位统一为 **每 1M tokens**（各厂商价目表
的通用单位）。`pricing` 属于**契约侧**的产品事实：它不进
`modelprovider.ModelDescriptor`（该契约明确"计费属于契约侧"），不进运行时
请求补丁，不参与可执行性判定。免费模型用 `free: true` 表达；`input=0 且
output=0` 也按免费展示，但文档里保留声明的数字。

**未知键一律拒绝**（`unknown field "inputPrice"`），包括 `cost` 这种别名：
Alpha 零兼容，旧方言不保留垫片。

模态声明是**执行事实**：附件分发按 `properties.inputFormat` 决定原生 part
还是文本块；未声明任何输入模态的模型按纯文本处理。`GET /v1/models` 的
`enforcement.media` 因此为 `true`（与 modality-input 车道对齐），客户端不再
把它当"仅记录"。

## 2. 默认值与"未声明"

- 字段缺失 = 继承（规则层）或未声明（最终视图）。界面把后者显示为
  **未声明**，不显示 0：`上下文未声明 · 输出未声明`、
  `价格未声明`（带 `notes` 时是 `价格未声明（notes）`）。`contextWindow=0`
  与 `maxOutputTokens.max=0` 是 resolver 的"不完整"默认值，界面同样按未声明
  显示，绝不把 0 当成事实。
- 编辑器的空白字段 = 清除：表单打开时已回填解析值，所以清空后保存写入
  `null`（上下文、输出上限、思考档），或对价格字段逐项写 `null`。表单没
  声明过、也没被清空的字段不写，避免一次保存凭空产生 clear。
- `modelprovider.ModelDescriptor` 的 128k/16k 是 **discovery 契约**的默认
  （Pi 同样如此），不属于本文的文档词汇；`modelcfg` 的可执行性仍要求
  `contextWindow > 0` 与 `maxOutputTokens.max > 0`。
- 视图里 `pricing` 缺失以 `omitempty` 省略；客户端据"字段不存在"渲染未声明，
  与 `free`/零价区分。

## 3. 凭据：引用词汇 + 直接录入 + 掩码预览

### 3.1 引用词汇（严格、闭合）

模型文档的 `access.ref` 只接受引用，**永远不接受字面量**：

- `${NAME}` / `env:NAME` → 环境变量名（`modelcfg` 与 `internal/host` 同一正则，
  由 `TestSecretRefVocabularyMatchesHost` 钉住）；
- `secret:NAME` → 租户密钥库条目；
- `access.env` 同类（只放环境变量名）。

解析顺序（`provider-credentials` spec，两处同一判定）：显式 `ref` → 环境变量
已设置 → 租户同名 secret（内置 provider 的 env 名兼作 secret 名）→
进程级 legacy key → 失败（fail closed，不发空凭据）。

### 3.2 直接录入（UI 便利，不产生字面量）

`PUT /v1/settings/providers/{id}/credential`：

1. 校验 provider id（models 文档 id 语法）与 body（`{"value": string}`，严格
   解码）；
2. **空值拒绝**：`value is required: a blank field never erases a stored
   credential`——忘掉一个凭据只有显式删除 secret 一种路径；
3. 派生**稳定**名字 `provider_<id>`（`[A-Za-z0-9_]` 以外的字符换成 `_`，
   可重复写入同一行，不累积）；
4. 交给现有加密租户密钥库（AES-256-GCM `SecretBox`，AAD 绑定 tenant+name）；
5. 只回答 `{providerId, secretName, secretRef: "secret:<name>", preview,
   updatedAt}`。

客户端拿到 `secretRef` 后写进文档的 `access.ref`，再走 `PUT
/v1/settings/models`。因此**字面量从不进入模型文档**，也不进日志、错误、
事件或界面状态：编辑器里的一次性输入框在服务端接受后立即清空，之后只显示
预览。

### 3.3 掩码规则（精确边界）

`store.SecretPreview`：

- 凭据 **rune 数 > 18** → `first6 + "…" + last6`；
- **≤ 18** → 固定掩码 `••••••••`（无前缀、无后缀）；
- 按 **码点** 计数（CJK/emoji 不按字节或 UTF-16 单元）；18 是全掩码、19 是
  第一个可预览长度（`18` 时 6+1+6 的可见部分与全值重合，所以边界取
  `>18`）。

预览在**写入时**计算并贴合到行（`tenant_secrets.preview`，additive、
nullable）。读取路径（列表、PUT 响应）只带预览，**永不**返回明文、长度、
hash 或任何其它可猜事实。旧行（`preview IS NULL`）的列表投影为空字符串，
界面显示 **已保存**，不反推。

## 4. 实时模型发现（B）

**不新增发现栈**：复用已有的 provider 探针
`POST /v1/providers/{id}/test`（`modelcfg.ProviderProbe`，
`servicehost.ProbeProvider` → `provider.FetchRemoteModels`）。

- 请求是各协议最便宜的 `/models`（Anthropic 为 `/v1/models`），凭据在**服务端**
  按与 attempt 相同的顺序解析，客户端从不见到 key；
- 结果是一份测量值：`ok`、`status`（`ready|needs_auth|unreachable|invalid_config`）、
  `latencyMs`、`modelCount`、`models[]`（上游 id，排序去重）、`error`（从不含
  URL/凭据）；
- 失败的测量仍是 200 `ok:false`；只有"探测根本跑不起来"（未知 provider 404、
  未接线 501）才是错误。

界面流程：Provider 编辑器「从服务端拉取模型」→ 探针 → **并入**编辑器列表：

- 去重按 **id 精确相等**（大小写不同是不同身份）；
- 新 id **追加在末尾**，已配置模型**一个不删**（上游清单变了不能静默删掉
  有人配过的东西）；
- 已配置模型不重复；新增数会 toast，清单一致时明确说"没有新增"。

**新 Provider 尚未保存**时服务端没有它的解析结果（不认识这个 id），界面引导
先保存（可带占位模型）再拉取；不发明"草稿探测端点"。

## 5. 逐模型编辑与投影

- `/v1/providers`：设置视图，含不完整/停用条目及其 issues；
- `/v1/models`：扁平注册表；`pricing` 随 `ModelView` 一起投影
  （`omitempty`），只用于展示与估算，**不参与执行**；
- 界面「参数」对话框写**一条** `providerModelRules` 精确规则，使用与解析器
  相同的 RFC 7386 合并语义：对话框编辑可见的思考档 `values`（运行时词汇、
  升序），也编辑模型家族（`providerType`；「继承」写 `null`），但不编辑各档
  的请求补丁 `map`；已有补丁原样保留，新增档位补一个空补丁；清除价格以
  `pricing: null` 表达，单项清空写该项 `null`；
- 对话框保存到文档的规则本身是"补丁的补丁"：组合时 `null` 必须保留（不能
  当作删除键），否则被清空的字段会在解析时重新继承下层规则；
- 默认模型选择器与 provider 卡片显示 `上下文 · 输出 · 价格` 摘要与 **免费**
  徽章；模型 id 只作为细节行，主标签永远是展示名。

五个界面状态齐备：加载（Spinner/骨架）、空（"还没有模型"）、错（探测/保存
的可读原因）、权限不足（现有 401/403 面）、成功（徽章与摘要随之更新）。

## 6. 安全边界（总结）

| 事实 | 存哪 | 谁能读 |
|---|---|---|
| 凭据明文 | 只进加密租户密钥库（`SecretBox`） | 只有服务端解析路径（内存中） |
| 模型文档 `access` | 只有引用（`env:NAME`/`secret:NAME`/`${NAME}`） | API 读者 |
| 掩码预览 | `tenant_secrets.preview` | 设置列表/编辑器 |
| 长度/hash/前缀 | **不存、不返回** | — |
| 探测结果 | 内存（客户端）与服务端响应 | 设置界面 |

直写凭据的路由与既有 `PUT/DELETE /v1/settings/secrets/{name}` 共用同一份
存储与同一套 503/语法拒绝；删除仍受"被模型文档引用即拒绝"保护。

## 7. 与 Pi / Magpie / ZCode 的对照

| 维度 | Pi | Magpie | ZCode | Keel（本文） |
|---|---|---|---|---|
| 模型字段 | `id/name/api/reasoning/input/contextWindow/maxTokens/cost{input,output,cacheRead,cacheWrite}/compat/samplingParams` | 逐模型 context/output 覆盖 + 预设（128K…1M） | provider 接入（OpenAI/Anthropic），大上下文 + input/cached/output 价 | `properties.contextWindow`、`options.maxOutputTokens`、`pricing{currency,input,output,cacheRead,cacheWrite,free,notes}` |
| 必填 | 只有 `id`；缺省 128k/16k、cost 0 | 预设一键套用 | 由 provider 声明 | `contextWindow>0` + `maxOutputTokens.max>0` 才可执行；价格全可缺省 |
| 实时清单 | `pi.dev/models` 目录 | provider 编辑器 + 本地清单 | provider 连接后拉清单 | 复用 `POST /v1/providers/{id}/test` 探针（只并入不删除） |
| 价格单位 | 每 1M tokens（`cost`） | 分层/闲时价 | 每 1M tokens | 每 **1M tokens**；四档 + 免费标记 + 备注 |
| 免费/零价 | cost 0 即零 | 免费/零标签 | — | `free: true` 或 `input=0且output=0`，界面徽章；未声明单独一态 |
| 凭据 | provider `api` 配置 | provider 编辑器 | provider 连接 | 严格引用词汇；UI 直写→加密库→只回引用；写入时掩码预览 |
| 掩码 | — | — | — | >18 rune 保留前 6/后 6，否则固定掩码；无长度/hash |

## 8. 测试与证据

- `internal/modelcfg/pricing_test.go`：词汇往返、逐字段合并、`null` 清除、
  未知键拒绝、免费-only、深拷贝、租户规则链路，以及"价格不进请求补丁、无价
  仍可执行"。
- `internal/store/secrets_preview_test.go`：18/19 边界、CJK/emoji 按码点、
  固定掩码不随值变化、预览不含全值；`secrets_test.go` 钉住写入时计算、覆盖
  轮换、旧行 NULL、跨租户。
- `internal/harnessapi/provider_credential_test.go`：直写路由的成功/短值/
  空值不擦除/租户门/严格 body/503/稳定名字。
- `internal/harnessapi/models_test.go`：`/v1/models` 与 `/v1/providers` 的
  `pricing` 投影（有/无两态）。
- `web/src/data/model-catalog.test.ts`：摘要、格式化、合并补丁、逐模型保存、
  思考档升序与请求补丁保留、清除字段写 `null`（且不把下层值放回来）、
  实时清单合并、掩码镜像。
- `web/src/screens/settings-models.test.tsx`、`settings-secrets.test.tsx`：
  TUI 的直写/拉取成功与凭据失败/参数编辑（含思考档与清空）/掩码预览，
  390 与 1440 两种宽度；保存后的文档形状经「高级」原始文档读取验证。
- `internal/harnessapi/provider_credential_test.go`：直写路由的成功/短值/
  空值不擦除/租户门/严格 body/503/稳定名字，以及"值不进模型文档、不进日志、
  不进错误"。
- 浏览器位图（390/1440 两档）见
  `openspec/changes/keel-harness-v2/evidence/model-catalog/`：设置页、
  provider 编辑器与直写凭据、服务端拉取、模型参数（顶部/思考档/价格）与
  会话模型选择器；provider 编辑器与模型参数各有 390 与 1440 两种宽度。
