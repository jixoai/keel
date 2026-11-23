# Plugin System 增量（keel-harness-v2 对既有 spec 的修改）

本 delta 只改与 v2 冲突的既有 Requirement；未列出的 Requirement 原样生效。

## MODIFIED Requirements

### Requirement: 三种 kind

插件 MUST 是三种**承载 kind** 之一（`in-process` / `sidecar` / `webview`），`entry` 形状 MUST 与
kind 匹配。kind 只描述**怎么承载**，不描述架构角色：

- `in-process`：随进程编译的官方插件；清单只控制启用状态与配置。
- `sidecar`：外部进程，stdio 换行 JSON-RPC（initialize/tools/list/tools/call/health/close），
  复用 gateway 的进程监督、env 白名单与进程组回收；崩溃只影响该插件（状态 failed，可重连）。
- `webview`：**UI 资源包**（`ui/`），由宿主按**槽位声明的形态**承载：WebView/iframe（任意 HTML），
  或 **NSH chunk**（原生渲染，与宿主同一 VM）。可带 sidecar 后端。

插件 MAY 另外声明 `layer`（`kernel` / `harness` / `provider` / `ui` / `service`）表达架构层；
`layer` 与 `kind` 正交，缺省按 kind 推导（`webview`→`ui`、`sidecar`→`provider`、
`in-process`→`harness`）。`kind` 仍是闭集，`ParseKind` 不接受第四个值。

#### Scenario: sidecar 崩溃隔离
- **WHEN** 某 sidecar 插件进程被 kill
- **THEN** 其工具调用返回明确错误、插件状态 failed；其他插件与 Harness 不受影响；可重新启用恢复

#### Scenario: webview 包的两种承载
- **WHEN** 一个 `webview` 插件声明 `ui/index.html` 与 NSH chunk 两种入口
- **THEN** 槽位允许集决定实际承载形态；两种形态共用同一清单、权限与审批，不新增 kind

#### Scenario: layer 缺省推导
- **WHEN** 清单只写了 `kind: sidecar` 而没有 `layer`
- **THEN** 其 layer 推导为 `provider`，装配与可见性按 provider 处理

### Requirement: 创造模式（作为内置插件 `keel.creation`）

创造模式 MUST 自身是插件（可停用），且 MUST：

- **聚合**本机所有已装插件（含第三方）的 `extensibility.json` + `EXTENSIBILITY.md`，形成机器可读
  目录，经只读发现工具交给 AI（AI 因此知道本机装了什么、能怎么扩展）；
- 以生成 **Native 插件**（NSH chunk 槽位贡献）为主路径，保留 webview+react+solid-glass 模板作为
  兜底；产物 MUST 可被槽位装配、渲染、卸载；
- 走脚手架 → 校验 → durable 审批 → 安装 → 启用；失败 MUST 回滚且无半装状态；
- 产物 MUST 可列举、可停用、可删除；生成/安装/升级 MUST 写审计；
- 用一条端到端用例证明 **AI 能写出 Native 插件**。

#### Scenario: 一句话生成 Native chunk 插件
- **GIVEN** 已启用 `keel.creation`
- **WHEN** 用户描述「给任务卡加一个显示耗时的部件」
- **THEN** 生成一个带 `chunk` 槽位贡献的插件包，安装并启用后任务卡上出现该部件；审计有
  install/enable 两条记录

#### Scenario: 一句话生成插件
- **GIVEN** 已启用 `keel.creation`
- **WHEN** 用户描述「做一个展示当前会话 token 用量的小面板」
- **THEN** 生成一个 webview 插件包、安装并启用；插件区出现该插件；打开 UI 能看到数据；
  审计有 install/enable 两条记录

#### Scenario: webview 兜底
- **WHEN** 目标槽位只接受 `iframe` 或插件需要非 NSH 的页面
- **THEN** 生成 webview 包并显式标注「非原生主题」，不伪装成 chunk

#### Scenario: 生成失败可回滚
- **WHEN** 生成或校验失败
- **THEN** 不产生半装状态；错误原样返回；临时产物被清理

### Requirement: TUI 行为

TUI MUST NOT 渲染 WebView/iframe 或 `package`（签名包独立 surface）形态的插件 UI；MUST 为这些
插件提供「打开」，经 `openUrl` 打开浏览器（Web 深链），宿主不支持时 MUST 显示可复制的 URL 文本。
**NSH chunk 贡献 MUST 在 TUI 渲染**（同一渲染器的原生视图），MUST NOT 被一律禁止。

#### Scenario: TUI 打开插件
- **WHEN** 在 TUI 点击一个 webview 插件的「打开」
- **THEN** 不出现 WebView/HTML 渲染；调用 openUrl（测试断言 URL）或展示 URL 文本

#### Scenario: TUI 渲染 chunk 贡献
- **WHEN** 一个 `chunk` 贡献被装配到 TUI 支持的槽位
- **THEN** 它以终端视图渲染，输入与焦点行为符合 TUI 契约

### Requirement: 官方插件套件覆盖矩阵

仓库 MUST 维护 `plugins/COVERAGE.md` 与 `plugins/keel.plugin-market/catalog.json`，并以
`catalog.json` 为**唯一机读真源**：每条 MUST 有 `id` / `keelPluginId` / `status` / `wave` /
`dshGroups[]`；`COVERAGE.md` MUST 由该真源生成或经脚本比对。
映射规则（不是 id 集合相等）：`COVERAGE.md` §A/§B/§C 的能力行 MUST 各映射到 `catalog.json` 的
`keelPluginId`（或 `absent` + 理由）；§B 的 bundle 行统一映射到 `keel.bundles`；§D/E/F/G 的
来源表不参与映射。`catalog.json` 每个条目 MUST 有 `status` 与 `wave`。本 change 的 W1 只承诺
v2 核心；v1 已有能力面的插件化按 catalog 的 `wave` 推进。设计期清单 `plugin-inventory.md`
只是 change 附录，MUST NOT 作为归档后的规范引用。
已合并/废弃的 id MUST 显式标记：

- `goal` / `todo` / `plan-mode` 并入 `keel.tasks`（all-in-one plan-plugin）；
- `team` → absent（另有结构方案）；
- `workspace` 并入 Group（放置与协作单元）；
- `guard` 并入 `keel.supervisor`；
- `keel.skill` / `keel.preset` 统一为 `keel.skills` / `keel.presets`。

一条脚本 MUST 能完成上述映射比对（COVERAGE 行 → `keelPluginId` → catalog 条目）并以非零退出报告差异。

#### Scenario: 矩阵无空行
- **WHEN** 审查 `plugins/COVERAGE.md`
- **THEN** 每个 DSH 能力组都有映射与状态，absent 行必须有理由

#### Scenario: 映射对账
- **WHEN** 运行对账脚本
- **THEN** `COVERAGE.md` §A/§B/§C 每一行都映射到 `catalog.json` 的 `keelPluginId`（或 `absent` + 理由）；
  `catalog.json` 每个条目都有 `status` 与 `wave`；差异逐条列出并以非零退出

#### Scenario: absent 有理由
- **WHEN** 审查任一 `absent` 行
- **THEN** 该行给出理由与替代能力，不允许空理由

### Requirement: 插件包与清单

插件 MUST 是一个目录 `<plugins-root>/<id>/`，含 `plugin.yaml`：

```yaml
id: vendor.name                # ^[a-z][a-z0-9._-]{1,63}$，全局唯一
version: 1.2.3                 # semver；同 id 只能有一个启用版本
kind: in-process|sidecar|webview
layer: kernel|harness|provider|ui|service   # 可选；缺省按 kind 推导
description: 一句话
engines: { keel: ">=0.2.0 <0.3.0" }
provides:
  tools: [name, ...]
  hooks: [event, ...]
  triggers: [id, ...]
  slots: [{ id, forms, arity, schema?, targets }]        # 本插件提供的槽位
  ui: { entry: ui/index.html, title: "..." }   # 仅当贡献含 iframe 时必填；纯 chunk 包可省略
  backends: [{ tool: skills, action: view, impl: ... }]   # 可选：实现既有 action 的后端
contributes:
  - { slot: <slot-id>, form: chunk|package|iframe|data, entry?: ui/settings.js, payload?: ... }
extensibility: { doc: EXTENSIBILITY.md, schema: extensibility.json }  # 官方插件必填，第三方可选
requires: [plugin-or-service-id, ...]
permissions: { fs: [workspace-write], net: [host...], secrets: [NAME...], tools: [name...] }
entry: { command: [...], args: [...], env: [...] }   # sidecar 必填；in-process 可为空
source: { type: builtin|local|git|npm, ... }
```

`webview` 的 `ui.entry` MUST 在贡献含 `iframe` 形态时必填；**纯 chunk 包 MUST 允许省略
`ui`**。`layer` 可选，缺省按 kind 推导（webview→ui、sidecar→provider、in-process→harness）。
`extensibility` 对官方插件 MUST 必填，对第三方 MAY 缺省（缺失时标记「不可被创造模式利用」）。

#### Scenario: 清单非法即拒绝
- **WHEN** `plugin.yaml` 缺 id/version/kind 或 id 不合文法
- **THEN** 安装失败并返回字段级原因；插件目录不被采用

#### Scenario: 版本门
- **WHEN** 插件的 `engines.keel` 与当前版本不匹配
- **THEN** 安装与启用都被拒绝，提示不匹配区间

#### Scenario: 纯 chunk 包可安装
- **WHEN** 一个 `kind: webview` 包只有 `contributes[form=chunk]` 而没有 `ui.entry`
- **THEN** 校验通过并可安装；装配后贡献在宿主树内原生渲染，不需要 `/plugins/<id>/`

### Requirement: 工具发布名 wire 安全

`provides.tools` MUST 接受两种声明写法：裸本地名（`greet`）或已限定名（`<plugin>.greet`）。
宿主发布的模型可见工具名 MUST 匹配 `^[A-Za-z0-9_-]{1,64}$`；发布 MUST 先把 `<plugin>` 与
本地名以 `.` 连接、再把所有 `[^A-Za-z0-9_-]` 字符归一为 `_`；已经合规的名字 MUST 逐字不变。
归一后的发布名 MUST 在安装时校验：不满足文法或两个声明归一后同名 MUST 以字段级错误拒绝。
**`spawn_subagent` 的 v1「兼容红线」MUST 废止**：模型可见工具面以 `tool-surface` 为唯一真源；
插件 `provides.tools` 仍按本规则归一。

#### Scenario: 归一后一次调用闭环
- **GIVEN** 一个 sidecar 插件 `demo.sidecar` 在清单与 `tools/list` 中都提供本地工具 `greet`
- **WHEN** 它被启用后模型面上出现工具
- **THEN** 可见名是 `demo_sidecar_greet`；以该名调用解析回本地工具 `greet`、在 sidecar 上真的执行、结果原样返回

#### Scenario: 不合规即安装拒绝
- **WHEN** 声明的工具归一后不匹配 `^[A-Za-z0-9_-]{1,64}$`（例如超长），或两个声明归一后同名
- **THEN** 安装返回 `provides.tools[i]` 字段级错误并且包不被采用（清单不落库、包不入受管目录）

### Requirement: UI 承载与桥接

插件 UI MUST 按**槽位**装载，贡献形态由槽位声明：`chunk`（宿主树内原生渲染）、`package`
（签名包独立 surface）、`iframe`（`/plugins/<id>/`，兜底且显式标注非原生主题）、`data`
（宿主消费的 JSON，如导航项/动作）。插件 UI MUST
经宿主桥 `keel.plugin.*`（读自身状态、调自身工具、请求审批、订阅事件）访问能力，MUST NOT
获得宿主任何密钥；主题 MUST 使用宿主令牌。`/plugins/<id>/` 不再是唯一承载方式。

#### Scenario: 插件 UI 可用
- **WHEN** 打开已启用插件的一个 `iframe` 贡献
- **THEN** 页面加载自 `/plugins/<id>/`，能显示自身状态并调用自身工具得到结果

#### Scenario: chunk 贡献在宿主树内
- **WHEN** 打开一个 `chunk` 贡献
- **THEN** 它作为宿主树里的组件渲染，无 iframe、无 `/plugins/<id>/` 请求，主题随宿主切换

### Requirement: 生命周期

插件状态机 MUST 拆成两台：

1. **包/进程级**（安装与代码代际）：`installed → verified → available → removed`；升级为同 id 的
   版本替换；HMR 为 `reloading → rolled_back`（失败回滚）。`generation` 是**进程级**代码代际，
   MUST NOT 按租户分叉。
2. **租户级**（启用与可见性）：`disabled → enabled → disabled`；依赖缺失 MUST 进入 `PENDING`
   （可见、带原因，不半装载），装配失败为 `FAILED`。`plugin_state(tenant_id, plugin_id, status,
   config_version, reason)` 记录该层；`status` ∈ {disabled, enabled, PENDING, FAILED}。

启用/停用 MUST 走 agentext 的依赖拓扑装载与**反序卸载**；工具/hook/trigger/UI 的注册副作用
MUST 在停用时全部撤销（无残留工具、无残留订阅、无残留触发）。

#### Scenario: 停用即撤销
- **GIVEN** 一个提供工具 `x` 与 webhook 触发的插件已启用
- **WHEN** 停用它
- **THEN** 模型侧不再有 `x`；触发器不再触发；插件区显示 disabled；无错误残留

#### Scenario: 依赖缺失不装载
- **GIVEN** 插件 B 声明 `requires: [A]`
- **WHEN** A 未启用即启用 B
- **THEN** B 进入 `PENDING` 并给出缺失依赖原因（不半装载）；A 启用后可转 `enabled`

## ADDED Requirements

### Requirement: 插件状态与租户作用域

插件**包安装** MUST 为进程级（二进制/文件/受管目录）；**启用、配置、可见性** MUST 为租户级：

```
plugin_state(tenant_id, plugin_id, status, config_version, reason)
```

`/v1/plugins` MUST 按调用者租户过滤；同一 Group 内不同租户的启用状态 MUST 互不影响；
插件工具面 MUST 按租户装配。`user` 放置不改变该语义。`generation` MUST 是**进程级**代码代际
（HMR/装载），MUST NOT 放进租户行；租户配置版本用 `config_version`。

#### Scenario: 租户各自启用
- **WHEN** 租户 A 启用插件 P，租户 B 未启用
- **THEN** A 的 `/v1/plugins` 与工具面包含 P，B 的都不包含；同机安装的包不重复

#### Scenario: 同组跨用户隔离
- **WHEN** 一个 `pack` Group 同时承载租户 A 与 B 的会话
- **THEN** A 的插件配置与可见性不泄漏给 B，禁用 A 的插件不影响 B
