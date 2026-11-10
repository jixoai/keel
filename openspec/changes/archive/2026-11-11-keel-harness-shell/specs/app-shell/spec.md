# Harness Shell（外壳与页面重建）

本 spec 定义 Harness 客户端的产品结构。所有布局与组件 MUST 来自 `keel-ui-kit` 的公共出口。

## ADDED Requirements

### Requirement: 应用外壳与信息架构

应用 MUST 采用 zcode 同构外壳：左侧会话栏（264px 起、可拖动、可折叠）+ 主区（≥320px）+
右侧状态栏（≥1280 显示，45% 展开、可收起）；窄屏 ≤767px 为单列/抽屉；
标题栏 h-12；全局 Command Palette（Cmd/Ctrl+K）承载会话切换/模型切换/设置/插件入口。

#### Scenario: 宽屏三区
- **WHEN** 1280 视口打开 Harness
- **THEN** 会话栏、对话列、右侧状态栏同屏；收起侧栏后主区占满且对话列按 zcode 宽度档位重排

#### Scenario: 窄屏单列
- **WHEN** 视口 ≤767
- **THEN** 只显示一个主视图（列表/对话/设置），返回路径显式；无横向滚动

### Requirement: 会话列表

会话行 MUST 显示标题（未命名＝「新会话 · 相对时间」）、预览、时间与状态（等待确认/失败/运行中），
MUST NOT 出现裸 session id、MUST NOT 出现任何演示/演示数据入口；支持搜索、新建、删除。

#### Scenario: 无演示入口
- **WHEN** 在任意页面全文检索「演示 / demo / fixture / 切到演示」
- **THEN** 产品 UI 无命中（fixture 仅 `?fixture=1` 开发态可用，且不渲染任何入口）

### Requirement: 对话面

对话 MUST 包含：用户气泡、助手内容（单一流式来源）、工具家族卡（摘要行 + 折叠详情、结果先行）、
审批/问询卡、错误横幅（输入框上方、可关闭可重试）、引导行（流末尾）、身份行（provider/model）、
回合操作（复制/重试/时间）。流式与已提交文本 MUST 同一条渲染路径。

#### Scenario: 错误可恢复
- **GIVEN** 回合失败
- **WHEN** 打开该会话
- **THEN** 输入框上方出现原因（逐字来自账本）与重试；重试后横幅消失、旧回合保留

### Requirement: 输入区

输入区 MUST 为 sticky dock：多行输入、附件、队列/引导切换、停止/发送、模式切换；
发送失败 MUST 把草稿与附件还给用户；空态建议 MUST 只预填不发送。

#### Scenario: 草稿不丢
- **WHEN** 发送失败
- **THEN** 输入的文本与附件原样回到输入区，错误由横幅说明

### Requirement: 设置面

设置 MUST 为分栏结构（icon rail 68→268 + 内容卡 + 面包屑 + 内容列 max-w-4xl），分区
至少：通用、外观、模型与 Provider、密钥、插件、快捷键、关于。
模型/Provider MUST 以结构化表单呈现；原始 JSON 只在「高级」折叠内。
密钥 MUST 只输入不回读。

#### Scenario: 配置不越界
- **WHEN** 浏览工作区（非设置面）
- **THEN** 除「本会话模型」chip 外不出现任何配置项

### Requirement: 插件面与 TUI 行为

插件面 MUST 列出已装/可装/创造模式入口与插件状态；在 Web 目标点击插件 MUST 打开其 WebView 页面；
在 TUI 目标点击插件 MUST NOT 试图内联渲染插件 UI，而是打开浏览器（`openUrl`），
无法打开时 MUST 给出可点击的 URL 文本。

#### Scenario: TUI 插件入口
- **WHEN** 在 TUI 中打开某个插件
- **THEN** 不渲染插件 Web UI；调用 openUrl（或打印 URL）；返回后 TUI 状态不变

### Requirement: 同等结构与回归

同一份代码 MUST 同时服务 web 与 TUI；既有 TUI 验收（7 条）MUST 保持绿，
且新增：外壳布局、设置分栏、插件入口三条断言。

#### Scenario: 结构一致
- **WHEN** 分别在 web 与 TUI 打开同一会话
- **THEN** 头部/消息/输入区层级一致（平台差异仅图标与悬停）
