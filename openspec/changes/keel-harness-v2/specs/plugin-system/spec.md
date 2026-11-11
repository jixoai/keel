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

仓库 MUST 维护 `plugins/COVERAGE.md` 与 `plugins/keel.plugin-market/catalog.json`，并 MUST 与
`openspec/changes/keel-harness-v2/plugin-inventory.md` **三面一致**（id 集合、状态、波次）。
每个 DSH 能力组 MUST 有映射与状态；`absent` MUST 给理由。已合并/废弃的 id MUST 显式标记：

- `goal` / `todo` / `plan-mode` 并入 `keel.tasks`（all-in-one plan-plugin）；
- `team` → absent（另有结构方案）；
- `workspace` 并入 Group（放置与协作单元）；
- `guard` 并入 `keel.supervisor`；
- `keel.skill` / `keel.preset` 统一为 `keel.skills` / `keel.presets`。

一条脚本 MUST 能比对三面为零差异（id 集合、状态、波次）。

#### Scenario: 矩阵无空行
- **WHEN** 审查 `plugins/COVERAGE.md`
- **THEN** 每个 DSH 能力组都有映射与状态，absent 行必须有理由

#### Scenario: 三面比对
- **WHEN** 运行三面比对脚本
- **THEN** `COVERAGE.md`、`catalog.json`、`plugin-inventory.md` 的 id 集合与状态完全一致；
  差异逐条列出并以非零退出

#### Scenario: absent 有理由
- **WHEN** 审查任一 `absent` 行
- **THEN** 该行给出理由与替代能力，不允许空理由
