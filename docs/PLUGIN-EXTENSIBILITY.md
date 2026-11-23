# Plugin Extensibility — 契约冻结（keel-harness-v2 / W0.5）

> 状态：**frozen 2026-11-22**。本文件是 W0.5 契约切片的一部分，冻结三个格式：
> `plugin.yaml` 的扩展字段、`extensibility.json`（机读）与槽位/贡献语义。
> 实现校验器：`internal/pluginpkg`（`manifest.go`、`extensibility.go`）；
> 规格来源：`openspec/changes/keel-harness-v2/specs/plugin-architecture/spec.md`、
> `specs/plugin-ui-extension/spec.md`、`specs/plugin-system/spec.md`。
>
> 原则：**Harness 不内置任何具体槽位**。Harness 只提供注册/注销、贡献装载、schema 校验与
> 目标降级；槽位 id 由插件声明，并以声明者的插件 id 为命名空间。

## 1. 词汇与正交关系

| 词 | 取值 | 含义 |
|---|---|---|
| `kind` | `in-process` / `sidecar` / `webview` | **承载方式**（闭集；三者之外不解析） |
| `layer` | `kernel` / `harness` / `provider` / `ui` / `service` | **架构角色**（闭集；可选，缺省由 kind 推导） |
| `form` | `chunk` / `package` / `iframe` / `data` | 贡献的**运行形态**（闭集） |
| `arity` | `single` / `multi` / `ordered` | 槽位的**数量语义**（闭集，声明槽位时必填） |
| `target` | `web` / `native` / `tui` | 槽位可用的**客户端目标**（闭集） |

`kind` 与 `layer` 正交：kind 说「怎么装」，layer 说「契约住在哪」。缺省推导：

```
webview     → layer: ui
sidecar     → layer: provider
in-process  → layer: harness
```

显式 `layer` 覆盖推导，但必须满足承载约束（见 §2.2）。

## 2. `plugin.yaml` 扩展字段

```yaml
id: vendor.panel                 # ^[a-z][a-z0-9._-]{1,63}$
version: 1.0.0                   # semver
kind: webview                    # in-process | sidecar | webview
layer: ui                        # kernel | harness | provider | ui | service（可选）
description: One line.

provides:
  tools: [panel_refresh]         # 安装期显式扩展工具面
  slots:                         # 本插件声明的槽位
    - id: vendor.panel.card      # 必须带本插件命名空间
      forms: [chunk, data]
      arity: multi
      targets: [web, native, tui]
      schema: { type: object }   # forms 含 data 时必填
  backends:                      # 只挂既有聚合 action，不新增 action 名
    - { tool: skills, action: view, impl: view }

contributes:                     # 向槽位投出的贡献
  - { slot: vendor.panel.card, form: chunk, entry: ui/card.js }
  - { slot: vendor.panel.card, form: data, payload: { title: Card } }
  - { slot: keel.ui.settings-section, form: chunk, entry: ui/settings.js }

extensibility:                   # 官方插件必填；第三方可选
  doc: EXTENSIBILITY.md
  schema: extensibility.json
```

### 2.1 严格性

- 未知字段、重复键、第二个 YAML 文档：一律字段级拒绝（`KnownFields(true)`，无未知字段回退）。
- `provides.slots[].id`：必须匹配 `^[a-z][a-z0-9._-]{2,127}$`、含 `.`、且前缀等于本插件 id。
  裸名 `route` 被拒绝（提示使用 `keel.ui.route`）。
- `forms` / `targets` 非空、闭集、去重；`arity` 必填且闭集。
- 槽位声明必须自洽：某个 target 上至少要有一个 form 可渲染。`package`/`iframe` 需要图形面，
  因此「只收 iframe、只声明 tui」是非法槽位。
- `contributes[].slot` 必须命名空间化；`chunk`/`package`/`iframe` 必须带 `entry`（包内相对路径，
  不得 `..` 或绝对路径）且不得带 `payload`；`data` 必须带非空 `payload` 且不得带 `entry`。
- 贡献到**本清单声明的槽位**时，form 必须在槽位允许集内；贡献到**外部槽位**时，装配期用
  `Environment.KnownSlot` 校验「槽位存在 + form 被接受」，未知槽位 fail-closed。
- `iframe` 贡献要求 `provides.ui.entry`（`/plugins/<id>/` 的 HTML 面）。
- `kind: webview` 允许**纯 chunk 包**省略 `provides.ui`：只要至少有一个 `contributes` 条目。
- `entry` 指向的文件必须真实存在于包内（`LoadDir` 的树校验）。

### 2.2 layer 与承载约束

| layer | 允许 kind | 理由 |
|---|---|---|
| `kernel` | `in-process` | loop seam 编译进内核进程 |
| `harness` | 任意 | 工具/服务在 Harness 装配 |
| `provider` | `sidecar` / `webview` | provider 在独立进程；webview 可带 sidecar 后端 |
| `ui` | `webview` | 客户端资源包 |
| `service` | `sidecar` / `in-process` | 独立服务（gRPC）；in-process 只承载客户端/注册配置，`webview` 不允许；gRPC 传输见 §8 defer |

`provides.provider.layer: provider` 仍是 provider 类型的专属声明；顶层 `layer` 是架构角色，
两者都出现时应当一致。

## 3. 槽位 id

### 3.1 第一方冻结集

以下 id 由第一方插件声明，**不是 Harness 内置清单**；`keel.ui` 卸载时它们与贡献一并撤销。

| 槽位 id | 声明者 | 允许形态 | 典型用途 |
|---|---|---|---|
| `keel.ui.route` | `keel.ui` | chunk / package / iframe | 页面/路由 |
| `keel.ui.nav` | `keel.ui` | data | 导航项 |
| `keel.ui.action` | `keel.ui` | data | 工具条动作 |
| `keel.ui.tool-renderer` | `keel.ui` | chunk | 工具调用渲染 |
| `keel.ui.message-part` | `keel.ui` | chunk | 消息部件 |
| `keel.ui.sidebar` | `keel.ui` | chunk / iframe | 侧栏面板 |
| `keel.ui.settings-section` | `keel.ui` | chunk | 设置分区 |
| `keel.ui.surface` | `keel.ui` | package / iframe | 独立表面/重型工具 |
| `keel.tasks.widget` | `keel.tasks` | chunk | 任务卡部件 |

第三方插件可以声明自己的槽位（如 `acme.analytics.panel`），命名空间必须等于其插件 id；
其他插件向该槽位投贡献时**不需要改 Harness**。

### 3.2 形态 × 目标

| form | 运行方式 | web | native | tui |
|---|---|---|---|---|
| `chunk` | NSH 动态导入，宿主树内组件 | ✅ | ✅ | ✅（ratatui 原生视图） |
| `package` | NSH 签名包，独立 surface + HostLimits | ✅ | ✅ | ❌ 显式不可用 |
| `iframe` | iframe / WebView | ✅ | ✅ | ❌ 显式不可用 |
| `data` | 宿主消费的 JSON | ✅ | ✅ | ✅ |

TUI 上的不可用必须**显式降级**：入口隐藏或显示「此面板在终端中不可用（需要图形界面）」，
并提供在浏览器中打开的入口；不得静默消失或渲染空白。

## 4. 能力与权限边界

- UI 只消费投影，不直写领域真值。读自身状态、调自身工具、请求审批、订阅事件一律经宿主桥
  （NSH `host.call` / `host.on`，命名空间 `keel.*`）。
- 宿主按插件 `permissions` 与声明裁剪可授予的 name 前缀；越权调用以 `ERR_ACCESS_DENIED`
  类型化失败并写审计。桥不暴露宿主密钥、凭据或跨租户数据。
- `chunk` 与宿主同 VM：只允许已安装并通过审批的插件；不为每个 chunk 新建 VM。
- `package` 由宿主设置 `HostLimits`（堆、加载时限、单次调用时限、节点上限），关闭即卸载 VM。
- `iframe` 是兜底形态：界面必须显式标注「非原生主题」，TUI 不可用。
- 主题令牌由宿主提供（solid-glass + shadcn 令牌）；插件不得硬编码颜色、字体、圆角、阴影或图标族。

## 5. `extensibility.json`（机读）

`extensibility.json` 是创造模式的聚合输入；`EXTENSIBILITY.md` 是人读版本。
契约版本：`keel.plugin-extensibility/1`。所有数组键**必须出现**（空用 `[]`），未知字段拒绝。

```json
{
  "schema": "keel.plugin-extensibility/1",
  "plugin": "keel.web-search",
  "version": "1.0.0",
  "summary": "One tool that searches the live web through a fallback chain.",
  "slots": [],
  "contributes": [],
  "configuration": [
    { "key": "KEEL_WEB_SEARCH_LIMIT", "type": "integer", "description": "Default result count (1–10)." }
  ],
  "hooks": [],
  "providers": [
    { "port": "web.search", "description": "Search provider consumed by the web plugin." }
  ],
  "examples": [
    { "title": "Search the web", "description": "Call the search tool with a query.", "snippet": "{\"query\":\"keel harness\"}" }
  ],
  "limits": [
    { "name": "total_budget_ms", "value": "30000", "description": "Whole fallback chain budget." }
  ]
}
```

字段规则：

| 字段 | 规则 |
|---|---|
| `schema` | 必须等于 `keel.plugin-extensibility/1` |
| `plugin` / `version` | 必须与 `plugin.yaml` 的 `id` / `version` 完全一致 |
| `slots` / `contributes` | 必须与 `plugin.yaml` 的 `provides.slots` / `contributes` 逐项一致（装配期比对） |
| `configuration[].key` | `^[A-Z][A-Z0-9_]{0,63}$`；唯一；`type ∈ string\|integer\|number\|boolean\|string_list\|object` |
| `hooks[].event` | 闭集 hook 事件名（与 `provides.hooks` 同语法） |
| `providers[].port` | 小写 feature port（与 `modelprovider.ValidFeaturePort` 同规则） |
| `examples[]` | `title` + `description` 必填 |
| `limits[]` | `name` + `value` + `description` 必填；`value` 是展示字符串（如 `30000`、`10 MB`） |

### 5.1 官方发布门禁

- 官方插件 MUST 在 `plugin.yaml` 声明 `extensibility.doc` + `extensibility.schema`，两个文件
  MUST 存在；缺任一字段或文件即拒绝发布（`Environment.RequireExtensibility`）。
- 第三方插件 MAY 省略：安装不受影响，但在市场与创造模式目录中标记「不可被创造模式利用」。
- `extensibility.json` 与 `plugin.yaml` 漂移（插件 id/版本/槽位/贡献不一致）即拒绝装载。

### 5.2 创造模式聚合协议

`keel.creation` 聚合本机**所有已安装插件**（含第三方）的上述文档，形成机器可读目录，经只读
发现工具交给 AI。目录与装配事实同源：插件启停后目录同步更新；卸载即从目录消失。
聚合口径与 `plugins/keel.plugin-market/catalog.json` 同源（单一真值）。

## 6. 四层装配与 row id overlay

装配是**四层叠加**，不是代码里的 if/else 分支：

```
base bundle        （默认装配：内核 seam + 记忆卫生 + 产品件）
   ↓ row id 覆盖
mode bundle        （serve / headless / tui / sdk）
   ↓ row id 覆盖
preset             （工具/提示/技能的行为覆盖）
   ↓ row id 覆盖
config.toml        （用户配置；可轮转）
```

- 每一行有稳定 **row id**（`<plugin-id>#<row>`，如 `keel.shell#bash-local`）。覆盖按 row id
  进行，**last write wins**；没有 row id 的行不参与覆盖。
- 覆盖只替换该 row 的实现/参数，不重排其他 row；启动顺序由「服务可用性」驱动，不手写。
- 例：`headless` 只把 `keel.ui#shell` 覆盖为 absent，base 的其余 row 原样保留；preset 关闭
  `bash` 只影响该会话的工具清单，不改写 base/bundle 定义。
- `config.toml` 轮转是**不可变版本 + 账本锚点**：写 `config.toml.new` → fsync → `rename()`
  原子覆盖；旧版按事件序号锚定。一次 run 固定使用一个版本（`configHash` + `anchorEventSeq`），
  中途轮转只影响下一次 run。
- 子代理 fork 继承父 run 的配置版本，随后可写自己的 `config.toml`。

当前 `internal/loadout` + `internal/servicegraph` 表达「服务图」这一层；base/mode/preset 的
row id 覆盖是**本契约**的要求，具体装配器在 W8/W9 落地（不在 W0.5 伪造）。

## 7. layer 边界与架构测试

依赖方向只能向下：

```
ui (web/**)  ──→  harness / kernel（经 HTTP/SSE 与宿主桥，不 import）
harness      ──→  provider / service / kernel
provider     ──→  kernel（seam 结构体/接口）
service      ──→  自身存储；不得 import harness/product
kernel       ──→  只依赖自身 + 标准库/第三方（不 import 任何上层）
```

`internal/architecture` 用 `go list -deps` 断言：

- kernel 包不得 import harness/provider/service/product（含 `layerfs`、`tasklog`、`fileplugin`、
  `placement`、`toolsurface`、`notify`、`gateway` 等）；
- harness 包不得 import `cmd/keel` / `cmd/runtime`（装配是命令的职责）；
- service（`layerfs`、`blob`）不得 import harness/product；
- provider 边界的 Go 侧是 `internal/gateway`（sidecar 协议），不得 import harness/product。
  **注意**：`internal/provider` 是内核 loop 使用的**进程内 LLM adapter**，属于 kernel seam，
  不是插件 provider 层；`internal/modelprovider` 是清单/设置共用的 provider 类型 schema，
  不是进程边界；
- 没有任何 Go 包 import `web/**`（UI 是独立客户端，只经协议通信）。

已登记的 Phase A 例外：`layerfs` 目前是进程内 Go CAS（`internal/layerfs`），Harness 直接
import；Rust + gRPC 传输与「service 只经 gRPC」的强约束见 §8 defer。架构测试只断言方向，
不把 Phase A 的进程内实现伪装成已完成的 gRPC 边界。

## 8. 明确不在本契约内（defer）

以下项**不在 W0.5 实现**，已登记到 `openspec/changes/keel-harness-v2/evidence/w05-contract.md`
的 backlog；不要用半成品代码冒充：

- macOS native target 产物（NSH 多平台构建直接产出，本切片不做）。
- NSH 0.5 升级（Window API / 媒体 grant / `focus-visible` / `activationPolicy`）。
- 桌面壳：托盘、全局快捷键、自更新/OTA。
- RSS 采样器（`scripts/acceptance-rss.sh`，≤500MB 协议）。
  （W0 RSS 切片已交付：`scripts/acceptance-rss.sh` + `scripts/lib/rss_sampler.py`，
  证据 `openspec/changes/keel-harness-v2/evidence/w0-rss.md`；本 W0.5 契约文本不再变更。）
- 多租户 `plugin_state(tenant_id, ...)` 与 `/v1/plugins` 租户过滤。
- layerfs Rust + gRPC（含 proto、健康检查、端口、SIGKILL sweep）。

## 9. 校验入口

```bash
go test ./internal/pluginpkg                 # 字段级校验器 + 官方文档门禁
go run ./plugins/tools/verifypkg             # manifest + 真 sidecar 握手 + 扩展面文档
node scripts/plugin-catalog.mjs --check      # catalog 唯一真源与 COVERAGE 对账
```

字段级错误路径示例：`provides.slots[0].forms[1]`、`contributes[2].form`、
`extensibility.schema: contributes[0].slot`。
