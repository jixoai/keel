# Tasks: keel-ui-kit

> T=令牌 · C=组件 · L=布局块 · G=Gallery/门禁 · Q=验收 · R=复核。
> 纪律：所有件先在 kit 内落地并过门，才允许被 screens 使用；数值对齐 zcode（见 proposal 参数表）。

## T. 令牌

- [x] T1 `web/theme.json` 换成 zcode `theme-zai-dark` 等价刻度（字号 14/12/10/9、圆角 6/12/16、
      背景三档、10% 边框、brand/accent/success/warning/danger、surface/hover/selected 叠色）
- [x] T2 `web/src/ui/kit/tokens.ts`：把 theme 暴露成类型化令牌（color/space/radius/typeScale/zIndex/motion）
- [x] T3 硬编码颜色扫描脚本（screens + kit）并入门禁

## C. 组件（按组并行）

- [x] C1 基础：Button（variant: default/secondary/ghost/outline/destructive/link + size sm/default/lg/icon）、Badge、Separator、Kbd、Spinner、Avatar、Skeleton
- [x] C2 表单：Input、Textarea、Label、Field（label/hint/error）、Switch、Checkbox、RadioGroup、Select、Combobox/Command
- [x] C3 容器：Card（header/content/footer）、ScrollArea、Collapsible、Accordion、Tabs、Table
- [x] C4 浮层与导航：Dialog、AlertDialog、Sheet、Popover、Tooltip、DropdownMenu、ContextMenu、Toast、Empty、Breadcrumb
- [x] C5 布局原语：Resizable（两栏/三栏，handle 4px + 指示线，键盘 16px 步长）、Sidebar（264/50%/折叠/持久化）、SidePane（45%/min240/max65%）
- [x] C6 每件：props 表（kit 内注释即文档）+ TUI 可渲染 + disabled/error 态

## L. 布局块

- [x] L1 `WorkspaceShell`（Z 参数逐项：264/50%/16px/4px 折叠/手柄/持久化/767 断点/自动折叠阈值/主区 min320）
- [x] L2 `SettingsShell`（icon rail 68→268 + 内容卡 rounded-xl + h-12 面包屑 + max-w-4xl + 分区记忆）
- [x] L3 `ConversationColumn`（三档宽度 + 1280 右移让位）+ `ConversationStatusRail`（≥1280 宽 320px）
- [x] L4 `ComposerDock`（sticky、输入壳 rounded-2xl/border/p-3/focus ring、回到底部圆钮位）

## G. Gallery 与门禁

- [x] G1 `/kit` gallery（三档视口、每组件三态、组件清单驱动）
- [x] G2 `scripts/kit-gate.sh`（nsh check + kit TUI 测试 + gallery 截图断言）
- [x] G3 `scripts/kit-imports.mjs`（screens 只准从 kit 公共出口导入；手搓壳布局失败）
- [x] G4 `web/kit/kit-registry.generated.tsx` **必须随提交入库**——它是 `web/` 与 `web/kit` 两处
      `nsh check` 的输入（fresh clone 缺它必红；`web/kit/.nsh/`、`web/kit/dist/` 才是生成缓存、不入库）。
      生成器 `web/kit/gen-registry.mjs` 幂等：`node web/kit/gen-registry.mjs`（或跑 `scripts/kit-gate.sh`）
      按 kit 当时落盘的导出重生成；缺件在 gallery 里显示为 `missing: <name>` 卡片

## Q. 验收

- [x] Q1 `nsh check` 0 错；`nsh test --target tui` 既有 7/7 + 新增 kit 用例全绿
- [x] Q2 gallery 在 390/768/1280 三档实拍，组件清单全覆盖、无裁切
- [x] Q3 布局参数逐项对照表（zcode 实测值 ↔ 实现值 ↔ 截图证据），偏差为 0 或显式登记
- [x] Q4 `kit-imports` 在含违规样例时非零退出（负控）

## R. 复核与交付

- [x] R1 子代理对抗复核（参数是否真的对齐、组件是否真被复用、门禁是否可绕过）
- [x] R2 报告 + 归档

## 已登记边界（交付时核实）

- gallery 只覆盖组件与布局块的**静态形态与三态**（390/768/1280 三档实拍）；交互路径由 `nsh test --target tui` 的 kit 用例覆盖，两者不重叠。
- nsh 0.4.0 的已知行为：≥3 面板组里 4px 面板的子内容不参与布局 → 折叠态改为「面板 visible=false + shell 左沿 4px 入口条」，因此折叠/展开没有 200ms 宽度动画（常量保留，展开后的宽度变化仍走它）。
- 终端平台无 hover、不绘制自定义手柄 → 键盘 16px 步长只能在 web/桌面验证，终端以原生 `role=adjustable` 承载。
