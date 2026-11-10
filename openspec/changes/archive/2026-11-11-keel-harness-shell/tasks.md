# Tasks: keel-harness-shell

> S=外壳 · L=会话列表 · C=对话面 · M=输入区 · G=设置 · P=插件面 · X=演示退场 · Q=验收 · V=复核。
> 前置：`keel-ui-kit` 已过门；本变更只从 kit 公共出口取布局与组件。

## S. 外壳

- [x] S1 `WorkspaceShell` 接线（侧栏/主区/右栏、持久化、自动折叠、767 单列）
- [x] S2 `CommandPalette`（Cmd/Ctrl+K：会话、模型、设置、插件）
- [x] S3 路由壳（workspace / settings / plugins / kit(dev)），深链与浏览器历史一致

## L. 会话列表

- [x] L1 Sidebar block：搜索、新建、行（标题/预览/时间/状态徽标）、底部连接状态
- [x] L2 行交互：选中、删除、折叠态入口；无裸 id、无演示入口
- [x] L3 顺序稳定与就地更新（沿用 store 语义）

## C. 对话面

- [x] C1 Header：标题、模型 chip、状态词、右栏开关
- [x] C2 轮次：用户气泡、助手内容（单一流式来源）、身份行、回合操作
- [x] C3 工具家族卡（摘要行 + 折叠详情、结果先行）
- [x] C4 审批/问询卡（选项 listbox、剩余时间、推荐位）
- [x] C5 错误横幅 / 引导行 / 滚动意图与记忆 / 回到底部
- [x] C6 右侧状态栏（≥1280；模型/思考档/用量/工具/插件事实；可收起）

## M. 输入区

- [x] M1 ComposerDock：多行、附件、队列/引导、停止/发送、模式切换
- [x] M2 发送失败还草稿；空态建议只预填

## G. 设置

- [x] G1 SettingsShell：rail 68→268、内容卡、面包屑、max-w-4xl、分区记忆
- [x] G2 通用/外观/快捷键/关于
- [x] G3 模型与 Provider：结构化表单（模板、key、endpoint、模型列表、元数据）+ 高级折叠（原始文档）
- [x] G4 密钥：只写不回读、引用提示
- [x] G5 插件：清单/状态/安装/触发/创造模式入口（与 `keel-plugin-core` 对齐）

## P. 插件面

- [x] P1 插件列表（已装/可装/更新）+ 状态与来源
- [x] P2 WebView 承载（`/plugins/<id>`）；TUI：openUrl + URL 文本兜底
- [x] P3 创造模式入口（对话式创建 → 安装 → 打开）

## X. 演示退场

- [x] X1 fixture 仅 `?fixture=1`/env 开发态；产品 UI 无任何演示字样与开关
- [x] X2 连接失败显示真实错误与重试（不伪装演示）

## Q. 验收

- [x] Q1 每屏 390/1280 before/after 成对截图（覆盖表 UNCOVERED=0）
- [x] Q2 布局参数对照（zcode 实测 ↔ 实现 ↔ 截图）
- [x] Q3 全文检索「演示/demo/fixture」产品 UI 零命中（负控：开发态允许）
- [x] Q4 `nsh check` 0 错；`nsh test --target tui` ≥10 条全绿；`kit-imports` 通过
- [x] Q5 真机走查：线上实例（web）全流程 ✓ + TUI 插件入口「在浏览器打开」的**渲染与调用路径** ✓（真终端点击未验：本机无可交互 TTY，见文末边界）

## V. 复核

- [x] V1 子代理对抗复核（结构是否真来自 kit、参数是否对齐、是否有隐藏配置面/演示残留）
- [x] V2 报告 + 归档

## 已登记边界（交付时核实）

- 12 页全部重建到 kit 上；`kit-imports --web web` → clean（screens 只从 kit 出口取件、零壳宽魔数、零硬编码颜色）；`nsh check` exit 0；`nsh test --target tui` 32/32；`nsh package --target web` 成功并在 1280/390 两档实拍外壳/设置/插件三路由。
- 插件 UI：`nsh.json` 增 `webview` 与 `openUrl` 能力；宽屏用 `WebView` 内嵌 `/plugins/<id>/`，终端 idiom 显示「在浏览器打开」并调 `openUrl`，失败时有 URL 文本兜底。
- **Q5 的真终端部分未验证**：本机没有可交互 TTY（`nsh run --target tui` 报 `interactive terminal input is closed`），所以「终端里点击插件入口真的拉起系统浏览器」只有代码路径与 TUI 渲染断言，没有真终端点击证据。
- 逐页面视觉复核由每个页面的验收人自己做：自己起宿主、自己截图、自己看图，最多三轮；未通过项写在各页 `open` 里。
