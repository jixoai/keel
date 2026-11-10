# Proposal: keel-ui-kit（NSH UI 库前置研发：shadcn 复刻 + zcode 布局原语）

## Why

NSH 是一个激进选择：它没有 DOM、没有 Tailwind、没有现成的 shadcn/ui。过去两轮客户端都直接把手写布局与
原生件堆到屏幕上，产出被 Owner 判为「半成品、程序员视角的工具」：配置乱飞、界面里还有演示模式、每个
页面的布局都是手搓的（侧栏比例、折叠、面板结构各写各的）。

根因不是某几个页面写得差，而是**没有一层经过验收的 UI 库**：没有可复用的布局块、没有与 zcode 对齐的
视觉令牌、没有任何组件级回归。任何「再打磨一轮」都会继续返工。

Owner 裁决（2026-11-10）：**任何要用的 shadcn/ui 组件都必须先用 NSH 复刻并测试，过门后才允许进入
Keel Harness**；布局不计成本地参考 zcode 的 layout/element/design。

## What Changes

1. **`web/src/ui/kit/` 组件库**：按 shadcn/ui（zcode 的 `radix-mira` + neutral + lucide 语汇）
   逐件复刻为 NSH 组件，**只覆盖本项目真的用到的件**，每件含：API（props 表）、三种状态（默认/禁用/错误或
   空）、TUI 可渲染、主题令牌取值。
   - 基础件：Button、Badge、Card、Separator、Input、Textarea、Label、Switch、Checkbox、RadioGroup、
     Select、Command（cmdk 语汇）、Dialog、AlertDialog、Sheet、Popover、Tooltip、DropdownMenu、
     ContextMenu、Tabs、Collapsible、Accordion、ScrollArea、Resizable、Sidebar、Breadcrumb、Avatar、
     Progress、Skeleton、Spinner、Toast/Sonner、Empty、Field、Kbd、Table。
2. **布局块（zcode 参数逐项对齐）**：
   - `WorkspaceShell`：侧栏默认/最小 264px、最大容器 50%、键盘步长 16px、折叠 4–6px/200ms、
     手柄 4px band + hover 0.5px 指示线、持久化（Storage key 对齐 zcode 语义）、
     自动折叠阈值（会话列 < 480 收右面板、< 360 收左栏，300ms 去抖）、单列断点 767px。
   - `SidePane`：展开 45%、收起 0、min 240px、max 65%。
   - `SettingsShell`：icon rail 68px → lg 268px、内容卡 rounded-xl + h-12 面包屑、内容列 max-w-4xl、
     分区记忆。
   - `ConversationColumn`：草稿/空态 max-w-2xl；≥864 max-w-4xl；≥1280 max-w-6xl 且左移 168px
     （为右侧状态栏让位；无容器查询时用 windowSize 断点等价实现）。
   - `ComposerDock`：sticky dock；输入壳 rounded-2xl / border / p-3 / focus ring。
3. **主题对齐**：`web/theme.json` 换成 zcode 的刻度——字号 14/12/10/9、圆角 6/12/16、
   zai-dark 取值（bg #161616、panel #202020、card #2b2b2b、border rgba(255,255,255,.1)、
   brand #fff、accent #001d3d）与 surface/hover/selected 的 5/10% 叠色。
4. **Gallery**（`/kit` 路由，仅开发/评审可进）：逐组件三档视口（390/768/1280）实拍，
   作为评审与回归的单一入口。
5. **门禁**：
   - `scripts/kit-gate.sh`：`nsh check` + kit TUI 测试 + gallery 截图（断言每个组件出现在三档视口）。
   - `scripts/kit-imports.mjs`：screens 只能从 `web/src/ui/kit` 的公共出口导入布局/组件；
     出现自绘布局（View 直接摆壳）即失败（白名单机制）。
6. **回归**：`web/App.tui.test.tsx` 既有 7 条仍绿；新增 kit 单测（组件级）。

## Non-Goals

- 桌面窗口 chrome（Windows 控件预留、mac 圆角分支）——Web/TUI 不适用。
- 右侧面板的标签拖拽排序（zcode 有 dnd-kit）；本轮只做 tab 条与溢出滚动。
- 终端面板（zcode 的 terminal panel）。
- zcode 的 `Streamdown` markdown 渲染全家桶；本轮用 NSH 现有文本渲染 + 代码块样式对齐，
  数学/mermaid 不在本轮。

## Impact

`web/`（新增 kit、theme.json、gallery、门禁脚本、测试）。服务端零改动。
后续 `keel-harness-shell` 的所有页面只能建立在本题的公共出口上。
