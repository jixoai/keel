# Plugin UI Extension（槽位、贡献与创造模式）

本 spec 定义插件如何扩展 UI。**槽位（slot）不由 Harness 内置，由插件提供**——最大的槽位提供者
是第一方客户端插件 `keel.ui`；Harness 只提供槽位注册与贡献装载的机制。UI 承载与桥的底座是
NSH（`nsh/base` + `host.call`），细节见 `docs/WEBUI.md`。

## ADDED Requirements

### Requirement: 槽位由插件声明，Harness 不内置槽位清单

Harness MUST 只提供槽位注册机制（注册/注销、贡献装载、schema 校验、目标降级），
MUST NOT 内置任何具体槽位。槽位 MUST 由插件声明，其 id MUST 以声明者命名空间为前缀
（`<plugin>.<name>`，如 `keel.ui.route`、`keel.tasks.widget`），MUST 避免全局裸名。

#### Scenario: ui 插件声明槽位
- **WHEN** `keel.ui` 启用并声明 `keel.ui.route`、`keel.ui.tool-renderer`、`keel.ui.sidebar`
- **THEN** 这些槽位出现在槽位目录中；卸载 `keel.ui` 时槽位与其贡献一并撤销，不留残缺 UI

#### Scenario: 第三方插件声明自己的槽位
- **WHEN** `keel.tasks` 声明 `keel.tasks.widget`
- **THEN** 第三方插件可向该槽位投贡献，命名不与其他插件冲突，无需改 Harness

### Requirement: 槽位声明与 Native 插件定义

槽位声明 MUST 包含：id、允许的贡献形态集合（`chunk` / `package` / `iframe` / `data`）、
贡献数量语义（单例 / 多值 / 有序）、数据 schema（若有）、支持的 target 集合、生命周期钩子。

**Native 插件**在本 spec 中定义为：**以 `chunk` 形态贡献、由 NSH 原生渲染的插件**（不是 iframe，
也不是仅 web 页面）。它 MUST 在 web target 与至少一个原生 target 上渲染一致；原生 target MUST
由 NSH 的多平台构建直接产出，MUST NOT 作为独立产品线另起工程。

#### Scenario: 声明缺字段
- **WHEN** 一个槽位声明缺少允许形态集合或 target 集合
- **THEN** 装配期拒绝该槽位并给出字段级原因

#### Scenario: Native 插件跨端
- **WHEN** 一个 chunk 贡献被装配
- **THEN** 它在 web 与至少一个原生 target 上渲染一致；TUI 按其 target 声明渲染或显式降级

### Requirement: 贡献形态三选，由槽位所有者决定允许集

贡献 MUST 是三种形态之一，且 MUST 只使用槽位声明允许的形态：

| 形态 | 运行方式 | 隔离 | 备注 |
|---|---|---|---|
| `chunk` | NSH 动态导入（`__nsh_import`），与宿主同一 VM，作为宿主树里的组件渲染 | 无（同崩溃/内存域） | 跨 target（含 TUI）；零额外 VM |
| `package` | NSH 签名包 `.nshpkg`，`run_package` 作为独立 surface 运行 | 独立 QuickJS VM + `HostLimits` + 能力强制 | 重型工具；独立表面，不是宿主树里的组件 |
| `iframe` | `<iframe>` / `WebView` 指向插件自托管页面 | 浏览器级隔离 | 兜底：非 NSH、存量 React/HTML 插件；不主题化、TUI 不可用 |

槽位所有者 MUST 显式列出它接受的形态；插件 MUST NOT 使用未列出的形态。

#### Scenario: 一个槽位只收 chunk
- **WHEN** 某槽位只允许 `chunk`，而某插件的贡献是 `iframe`
- **THEN** 装配期拒绝该贡献并给出字段级原因，界面不出现半成品入口

#### Scenario: 槽位允许 iframe 是槽位所有者的决策
- **WHEN** 一个重型工具槽位声明接受 `package` 与 `iframe`
- **THEN** 插件可自行选择签包（原生、隔离）或 iframe（存量页面），两者都合法

### Requirement: 贡献的生命周期可逆且失败隔离

贡献的装载与卸载 MUST 可逆且无残留（与插件副作用同规）；单个贡献装载失败 MUST NOT 影响
宿主 UI 与其他贡献，MUST 在面板与日志中可见。贡献 MUST 支持懒加载：不做首屏渲染的贡献
MUST 在其槽位首次使用时才加载。

#### Scenario: 贡献失败不拖垮宿主
- **WHEN** 一个 `chunk` 贡献在渲染时抛错
- **THEN** 宿主界面正常，该槽位显示可解释的失败态，其他插件的贡献不受影响

#### Scenario: 卸载即消失
- **WHEN** 停用一个提供侧栏面板的插件
- **THEN** 侧栏入口、面板会话与其事件订阅在同一 generation 内撤销

### Requirement: VM 与配额纪律

`package` 形态 MUST 由宿主设置 `HostLimits`（远小于 NSH 默认的 256MB 堆），MUST 声明并执行
堆、加载时限、单次调用时限与节点上限；`chunk` 形态共享宿主 VM，MUST 由装配期信任策略约束
（只允许已安装并通过审批的插件），MUST NOT 为每个 chunk 贡献新建 VM。`package` 贡献 MUST
在关闭时卸载 VM，MUST NOT 常驻。

#### Scenario: 打开一个重型面板
- **WHEN** 用户打开一个 `package` 贡献的面板
- **THEN** 该 surface 以声明的配额启动；关闭后 VM 被卸载，内存回落可测

#### Scenario: 超配额
- **WHEN** 一个 `chunk` 贡献触发宿主 VM 的内存/节点上限
- **THEN** 宿主按 NSH 的语义中止该次调用并报告，宿主进程不崩溃

### Requirement: 主题与组件来源

插件 UI MUST 使用宿主 UI kit（solid-glass 材质与主题令牌）绘制，MUST NOT 自带视觉语言
（颜色、字体、圆角、阴影、图标族）。主题令牌 MUST 由宿主经桥或共享模块提供，MUST NOT 由插件
自行硬编码。`iframe` 形态 MUST 在界面中显式标注「非原生主题」，MUST NOT 伪装成原生面板。

#### Scenario: 令牌随主题切换
- **WHEN** 宿主切换深浅色或主题令牌
- **THEN** 已装载的 chunk 贡献随之变化，无需重载插件

#### Scenario: 自带视觉被拒
- **WHEN** 官方插件的贡献使用硬编码色值与自定义阴影
- **THEN** UI 走查判不合格并返工（对齐项目「UI 组件禁手写、视觉语言同源」纪律）

### Requirement: 桥与能力

插件 UI MUST 只经宿主桥（NSH `host.call` / `host.on`，命名空间 `keel.*`）访问能力：读自身
状态、调用自身工具、请求审批、订阅事件。宿主 MUST 按插件声明裁剪可授予的 name 前缀，
MUST NOT 暴露任何宿主密钥、凭据或跨租户数据。桥调用 MUST 类型化失败（`ERR_ACCESS_DENIED`
/ `ETIMEDOUT` / 插件自身错误）并可被插件处理。

#### Scenario: 越权访问被拒
- **WHEN** 一个只声明了 `keel.plugin.self.*` 的插件调用 `keel.admin.*`
- **THEN** 调用以 `ERR_ACCESS_DENIED` 失败并记录审计

#### Scenario: 插件调用自身工具
- **WHEN** 面板里点击「重新索引」，插件经桥调用自己的工具
- **THEN** 结果原样返回面板，工具调用在任务日志中留下事实

### Requirement: 目标降级

槽位 MUST 声明支持的 target 集合（web / native / tui）。在不支持的目标上，宿主 MUST 显式
降级（入口隐藏或标注不可用），MUST NOT 静默消失或渲染空白。`iframe` 与 `package` 形态在 TUI
MUST 视为不可用（TUI 无 WebView），`chunk` 形态 MUST 在所有 NSH 目标上可用。原生 target
MUST 由 NSH 的多平台构建直接产出；本 change MUST NOT 为原生 target 另起构建/签名产品线。

#### Scenario: TUI 中的不可用贡献
- **WHEN** 在 `keel tui` 中打开一个只有 `iframe` 贡献的槽位
- **THEN** 界面显示「此面板在终端中不可用（需要图形界面）」，并提供在浏览器中打开的入口

### Requirement: 创造模式的聚合协议

每个插件 MUST 随包提供 `EXTENSIBILITY.md`（人读）与 `extensibility.json`（机读），声明它提供
的槽位、接受的配置、可挂 hook、可替换 provider、示例与限制。**`keel.creation` MUST 聚合
本机所有已安装插件（含第三方）的这些文档**，形成机器可读目录，并经只读发现工具交给 AI；
聚合目录 MUST 与装配事实同源（插件启停后目录同步更新）。

#### Scenario: AI 知道本机有什么可扩展
- **WHEN** AI 在创造模式下阅读聚合目录
- **THEN** 它能看到本机已装插件的扩展点（槽位、配置、hook、provider）与用法示例，据此写自定义

#### Scenario: 缺文档即不合格
- **WHEN** 一个插件没有 `extensibility.json`
- **THEN** 它在市场中标记为「不可被创造模式利用」，且官方插件发布门禁拒绝该包

### Requirement: Native 插件的可写性验收

创造模式 MUST 用一条端到端用例证明 **AI 能写出 Native 插件**：AI 从聚合目录出发，生成一个
带 `chunk` 槽位贡献的插件包（含清单、`extensibility.*`、贡献代码），经校验与审批安装后，
在 NSH 客户端（**web 与至少一个原生 target**）中渲染出该贡献，且资产与主题正确；同时给出
TUI 的渲染或显式降级证据。

#### Scenario: AI 现场写插件
- **WHEN** 给创造模式一个需求「给任务卡加一个显示耗时的部件」
- **THEN** 它产出一个插件包，安装后任务卡上出现该部件，卸载后无残留

#### Scenario: 校验拒绝坏包
- **WHEN** AI 生成的包清单非法、贡献形态不被槽位接受、或令牌硬编码
- **THEN** 校验器在安装前拒绝并给出字段级原因，AI 能据错误自行修正
