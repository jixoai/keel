# ui-kit Specification

## Purpose
TBD - created by archiving change keel-ui-kit. Update Purpose after archive.

## Requirements

### Requirement: 组件复刻清单与状态完整

`web/src/ui/kit` MUST 以 shadcn/ui（zcode 的 `radix-mira` + neutral + lucide 语汇）为唯一来源，
逐件复刻项目实际使用的组件；每件 MUST 提供：props 表、默认/禁用/错误（或空）三种状态、
键盘可达性（桌面与 TUI 均可操作）、主题令牌取值（不出现硬编码 hex）。

#### Scenario: 组件在 TUI 与 Web 同源
- **WHEN** 同一个屏幕在 `nsh run --target tui` 与 web 产物中渲染
- **THEN** 页面骨架、层级与交互一致（平台差异只允许图标位图/悬停态这类不可移植项）

#### Scenario: 组件不可用状态可达
- **WHEN** 组件被置为 disabled 或 error
- **THEN** 视觉与可访问性都表达该状态（不靠颜色单一通道），TUI 中表现为不可聚焦

### Requirement: 外壳与面板参数与 zcode 对齐

`WorkspaceShell` / `SidePane` MUST 采用 zcode 实测的外壳参数：

| 项 | 值 |
|---|---|
| 侧栏默认/最小宽 | 264px / 264px |
| 侧栏最大宽 | 容器宽 50% |
| 侧栏键盘步长 | 16px（聚焦手柄时左右键） |
| 侧栏折叠态 | 4px（Windows 6px），200ms width/opacity 过渡 |
| 拖拽手柄 | 4px band，hover/active 时出现 0.5px 指示线，`ew-resize` |
| 侧栏持久化 | Storage 键 `zcode:workspace-shell:sidebar-width-px` 语义（迁移旧键） |
| 主区最小宽 | 320px |
| 右侧面板 | 展开 45%、收起 0、min 240px、max 65% |
| 单列断点 | 767px（≤767 为单列/抽屉） |
| 自动折叠 | 会话列 <480px 收右面板；<360px 收左栏；300ms 去抖 |
| 标题栏/工具条 | h-12（48px） |

#### Scenario: 侧栏可拖动且持久化
- **GIVEN** 宽屏（≥1024）
- **WHEN** 拖动侧栏手柄
- **THEN** 宽度实时变化并被 clamp 在 [264, 容器 50%]；刷新后宽度保持

#### Scenario: 自动折叠
- **GIVEN** 右面板展开、会话列 460px
- **WHEN** 窗口继续变窄
- **THEN** 右面板先收起；会话列 <360px 时左栏也收起（300ms 去抖，无抖动）

#### Scenario: 折叠态仍可见入口
- **WHEN** 侧栏折叠为 4–6px
- **THEN** 悬浮/点击该条可重新展开，且不占用主区点击

### Requirement: 内容列与设置参数与 zcode 对齐

`ConversationColumn` / `SettingsShell` / `ComposerDock` MUST 采用 zcode 实测的内容参数：

| 项 | 值 |
|---|---|
| 对话列宽 | 草稿/空态 max-w-2xl；≥864 max-w-4xl；≥1280 max-w-6xl 且左移 168px |
| 设置栏 | icon rail 68px → lg 268px；内容卡 rounded-xl + h-12 面包屑；内容列 max-w-4xl |
| 面板圆角 | 6px（平台分支 ≤6）；手柄内缩 = 圆角 + 4 |
| 输入区 | sticky dock；输入壳 rounded-2xl / border / p-3 / focus ring |

#### Scenario: 对话列三档
- **WHEN** 视口 800 / 1200 / 1440
- **THEN** 对话列依次为 max-w-4xl / max-w-4xl / max-w-6xl 左移档位，且不与右侧状态栏重叠

#### Scenario: 设置栏展开
- **WHEN** 视口 ≥lg
- **THEN** 设置左栏 268px、内容卡与面包屑齐备、内容列不超过 max-w-4xl

### Requirement: 视觉令牌对齐

`web/theme.json` MUST 提供 zcode `theme-zai-dark` 的等价令牌：字号刻度（base 14 / sm 12 / xs 10 /
2xs 9）、圆角（6/12/16）、背景三档（bg / header·panel·tab / card·popover·input·menu）、
边框（10% 白）、brand、accent、success/warning/danger 与 surface/hover/selected 叠色。
页面 MUST NOT 出现硬编码 hex（除 theme 文件本身与图标位图）。

#### Scenario: 无硬编码颜色
- **WHEN** 审查 `web/src/screens/**` 与 `web/src/ui/kit/**`
- **THEN** 颜色全部来自主题令牌（脚本可检）

### Requirement: Gallery 与三档视口

MUST 提供 `/kit` gallery：每件组件以 390/768/1280 三档视口渲染，含默认/禁用/错误状态；
gallery MUST NOT 出现在产品导航中（仅开发/评审入口）。

#### Scenario: 组件全覆盖
- **WHEN** 打开 `/kit` 并用 390/768/1280 各截一张
- **THEN** 组件清单里的每一件都出现且无溢出/裁切

### Requirement: 门禁与消费者约束

MUST 提供 `scripts/kit-gate.sh`（`nsh check` + kit TUI 测试 + gallery 三档截图断言）与
`scripts/kit-imports.mjs`（screens 只能从 kit 公共出口导入；出现手搓壳布局即失败）。
`nsh test --target tui` 既有 7 条 MUST 保持绿。

#### Scenario: 违规手搓布局被拦
- **WHEN** 某个 screen 直接摆出侧栏/面板壳（不经 kit）
- **THEN** `kit-imports` 以非零退出并指出文件与行

#### Scenario: 组件回归被拦
- **WHEN** 某个kit 组件在 TUI 下渲染异常或 gallery 缺件
- **THEN** `kit-gate.sh` 非零退出
