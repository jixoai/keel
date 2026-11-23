# client-shell Specification

## Purpose
TBD - created by archiving change keel-harness-v2. Update Purpose after archive.

## Requirements

### Requirement: NSH 0.5.0 基线与破坏性变更

客户端 MUST pin NSH **0.5.0**（或兼容的更高 minor），并 MUST 处理其破坏性变更：

- **Window API 已替换**：桌面窗口 MUST 使用 `WindowRef` / `<Window>` 组件 / window kinds；
  MUST NOT 依赖旧 window API（未提供别名）。
- **媒体加载受 capabilities 约束**：声明 `capabilities` 的应用展示本地文件 MUST 有 `fs.read`
  grant；`new URL("./x.png", import.meta.url)` MUST 有 `${pluginDir}` grant；相对媒体路径在原生
  应用 MUST 视为不加载。
- **`focus-visible:` 只跟随键盘焦点**：点击/触摸不再触发；需要任意焦点样式 MUST 用 `focus:`。
- **macOS `LSUIElement` 被拒**：MUST 改用 `macos.activationPolicy`。

#### Scenario: 升级后窗口仍可开
- **WHEN** 客户端在 0.5.0 上打开任务板/交付物的独立窗口
- **THEN** 使用 `WindowRef`/`<Window>`，无旧 API 调用；窗口键位绑定仍属于正确的窗口

#### Scenario: 媒体授权缺失即拒绝
- **WHEN** 一个插件 UI 展示本地图片但没有 `fs.read`/`${pluginDir}` grant
- **THEN** 图片不加载并给出可见原因，不静默空白

### Requirement: 桌面应用模型

桌面客户端 MUST 支持：**单实例**、**系统托盘**、**全局快捷键**、**URL Scheme 深链**（打开指定
会话/任务/审批）、**文件关联**、启动隐藏与 macOS 菜单栏模式（`activationPolicy`）。多窗口 MUST
用 0.5.0 的窗口模型；任务板、交付物、插件 `surface` MAY 作为独立窗口，MUST NOT 假定只能单窗口。

#### Scenario: 单实例二次启动
- **WHEN** 用户再次启动客户端
- **THEN** 不创建第二个进程；已有窗口被聚焦，深链参数被投递给它

#### Scenario: 托盘与全局快捷键
- **WHEN** 主窗口隐藏时按下全局快捷键
- **THEN** 客户端按配置显示/聚焦主窗口；托盘事件报告图标位置，可把伴随窗口放在菜单栏项旁

#### Scenario: 深链打开审批
- **WHEN** 用户点击 `keel://session/<id>/decision/<id>`
- **THEN** 客户端打开对应会话并定位到该审批卡

### Requirement: 系统集成

通知 MUST 走系统通知（支持 silent），并 MUST 维护应用 **badge**（未读/待审批数）；通知点击
MUST 能深链到会话/审批。WebView MUST 支持 `evaluate` 返回值、应用自有 URL scheme、请求头、
incognito，以及摄像头/麦克风（平台不支持时 MUST 显式降级）。MUST 按 `capabilities` 授予，
MUST NOT 绕过 grant。

#### Scenario: 审批系统通知
- **WHEN** 一个审批在客户端离线时超时未答
- **THEN** 发系统通知与 badge；点击直达该审批；ack 后 badge 回落

#### Scenario: WebView 能力
- **WHEN** 插件面板调用 `evaluate` 或请求摄像头
- **THEN** 返回值可 await；未声明的能力被拒并给类型化错误

### Requirement: TUI 是一等 target

TUI MUST 由 NSH 的 ratatui 后端承载（`--target tui`），MUST 渲染 `chunk` 贡献；`package` 与
`iframe` 在 TUI MUST 显式不可用并说明原因。TUI MUST 支持键盘导航、选择、焦点与无障碍；
`Terminal` 组件 MAY 用于在应用内嵌终端输出（与把整个应用跑在 TUI 后端是两件事）。

#### Scenario: TUI 渲染任务板
- **WHEN** 在 `--target tui` 下打开任务板槽位
- **THEN** `chunk` 贡献以终端视图渲染，键盘可导航；`iframe` 入口显示「终端不可用」

#### Scenario: 内嵌 Terminal
- **WHEN** 在图形界面查看后台作业输出
- **THEN** 可用 `Terminal` 组件渲染程序输出，不必自带终端模拟器

### Requirement: 更新与分发

桌面客户端 MUST 支持自更新（check / download / install / restart）与**签名 OTA 包**
（`nsh/updater`）：MUST 校验签名、app id、版本与 `minRuntime`；OTA 包 MUST 支持试验期与失败回滚
（崩溃/致命错误回到 shipped bundle）。只改 TSX MUST 走 OTA；运行时变化 MUST 走完整应用更新。

#### Scenario: 自更新
- **WHEN** `checkForUpdate` 发现更新
- **THEN** 下载校验 SHA-256 后安装并重启；失败保留可回退路径

#### Scenario: OTA 回滚
- **WHEN** 一个 OTA 包在试验期崩溃
- **THEN** 下次启动回到 shipped bundle；信任库不接受该 build

### Requirement: 长会话与性能

客户端 MUST 使用 NSH 0.5.0 的长列表/滚动能力：长会话保持滚动位置、离屏内容增量创建、大表格
避免重复文本布局、大视图树消失后释放内存。1000 条消息的会话 MUST 保持可交互，并满足 G4 内存门。

#### Scenario: 1000 条消息滚动保真
- **WHEN** 在长会话中向上滚动并收到新消息
- **THEN** 视口不跳、贴底才跟随；离屏消息按需创建，内存不随历史线性增长

### Requirement: 编辑器与 Markdown

Composer MAY 使用 `RichEditor`（HTML 导入/导出）；消息渲染 MUST 使用流式 Markdown；代码/Diff/
终端 MUST 使用独立字号设置。MUST NOT 用普通 textarea 承载富文本或 Markdown 编辑。

#### Scenario: 粘贴富文本
- **WHEN** 用户把一段 HTML 粘进 composer
- **THEN** RichEditor 正确导入并可导出 HTML；纯文本路径仍可用

#### Scenario: 流式 Markdown
- **WHEN** 助手流式输出包含代码块/表格
- **THEN** 渲染不闪烁、不重排整段；代码块用独立等宽字号

### Requirement: 全局系统栏与日常章节

客户端 MUST 用一条全局系统栏承载**系统级**入口：品牌/窗口拖动区、插件 top-band 槽位、
声音开关与设置、通知面板、明暗主题切换、命令面板；窗口控制 MUST 仅在宿主明确暴露
隐藏标题栏绘制区（`nsh/desktop.titleBar()` 且未保留原生控制宽度）时渲染，MUST NOT
在 Web/TUI 或系统已绘制控件时画假按钮。宿主保留原生控制宽度时，系统栏左侧 MUST
为其留出空间，品牌不得压在原生控件下。

日常导航 MUST 只包含工作区 / 员工 / 团队三个章节（桌面 rail 与手机底部 tab 同一口径）；
身份 / 记录 MUST 作为设置页的“管理入口”，保留原路由、权限门与可访问名称，MUST NOT
作为日常主导航项。手机 390 MUST 保持三个日常 tab + 独立设置入口；390/768/1024/1440
MUST 无横向溢出。插件 top-band 贡献 MUST 继续渲染在系统栏内，无贡献时不得留下空白假面板。

#### Scenario: 桌面系统栏
- **WHEN** 读者打开工作区
- **THEN** 顶部只有一条系统栏；rail 只有工作区/员工/团队；身份/记录在设置的管理入口

#### Scenario: 宿主不支持应用绘制窗口控制
- **WHEN** 客户端跑在 Web/TUI，或原生宿主使用系统标题栏
- **THEN** 系统栏不出现最小化/最大化/关闭按钮，也不调用宿主窗口控制 API

#### Scenario: 手机导航
- **WHEN** 视口宽 390
- **THEN** 底部只有三个日常 tab 与设置入口；身份/记录仍可从设置进入

### Requirement: 字段高度与控制对齐

Web 客户端的工具条/筛选行/表单 MUST 使用同一套 kit 控制高度：桌面 34、手机 44。
文本输入、选择器、范围分段、检索框与同一行的按钮 MUST 同高、基线对齐，MUST NOT
让原生输入面（例如 19px）落在更高的外壳里。kit 的样式合并 MUST 保留组件默认值，
显式 `undefined` 不得擦除高度。Field 的 label/control/hint MUST 有固定间距，不得重叠。

表格 header MUST 为 34 + 细分隔线，正文行 MUST 为 44；数字列 MUST 右对齐。移动端
交互目标 MUST ≥44×44，390/768/1024/1440 MUST 无横向溢出。对话框 footer MUST 在同一
对话框内统一高度，并允许长表单滚动到底部。

#### Scenario: 记录筛选行
- **WHEN** 打开记录页的筛选行
- **THEN** 范围、Select、检索框与外层控件同为 34（手机 44），原生输入面与外壳同高

#### Scenario: 长表单对话框
- **WHEN** 在 390 宽度打开长表单对话框
- **THEN** 表单可滚动到 footer，footer 按钮同高且可点，无横向溢出

#### Scenario: 表格数字列
- **WHEN** 团队/群组表渲染成员与能力计数
- **THEN** header 与数字列右对齐，正文行 44，至少 34px header 高度
