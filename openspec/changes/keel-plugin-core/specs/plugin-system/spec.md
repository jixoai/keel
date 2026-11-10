# Plugin System（一切皆插件）

本 spec 定义 keel 的插件协议、生命周期、承载与权限。术语与语义对齐 DeepSeek Harness 的
「Everything is a Plugin」：插件由**服务可用性驱动装载**、**注册即可逆副作用**、能力面可替换。

## ADDED Requirements

### Requirement: 插件包与清单

插件 MUST 是一个目录 `<plugins-root>/<id>/`，含 `plugin.yaml`：

```yaml
id: vendor.name                # ^[a-z][a-z0-9._-]{1,63}$，全局唯一
version: 1.2.3                 # semver；同 id 只能有一个启用版本
kind: in-process|sidecar|webview
description: 一句话
engines: { keel: ">=0.2.0 <0.3.0" }
provides:
  tools: [name, ...]           # 工具名（全局唯一，冲突 fail-closed）
  hooks: [event, ...]          # 闭集事件
  triggers: [id, ...]
  ui: { entry: ui/index.html, title: "..." }   # webview 必填
requires: [plugin-or-service-id, ...]          # 依赖；缺失则不装载
permissions: { fs: [workspace-write], net: [host...], secrets: [NAME...], tools: [name...] }
entry: { command: [...], args: [...], env: [...] }   # sidecar 必填；in-process 可为空
source: { type: builtin|local|git|npm, ... }
```

#### Scenario: 清单非法即拒绝
- **WHEN** `plugin.yaml` 缺 id/version/kind 或 id 不合文法
- **THEN** 安装失败并返回字段级原因；插件目录不被采用

#### Scenario: 版本门
- **WHEN** 插件的 `engines.keel` 与当前版本不匹配
- **THEN** 安装与启用都被拒绝，提示不匹配区间

### Requirement: 发现、安装与包存储

MUST 支持从 `plugins-root` 发现插件；MUST 提供安装（本地目录/git/包）、卸载、列表 API；
安装 MUST 校验清单、版本门、包内容 hash 并写审计。插件包内容 MUST 存于受管目录
（不允许指向任意外部路径）。

#### Scenario: 安装即出现
- **WHEN** 安装一个合法插件包
- **THEN** `GET /v1/plugins` 立即列出它（状态 installed/enabled），UI 清单同步刷新

### Requirement: 生命周期

插件状态机 MUST 为 `installed → enabled → disabled → uninstalled`（升级为同 id 的版本替换）；
启用/停用 MUST 走 agentext 的依赖拓扑装载与**反序卸载**；工具/hook/trigger/UI 的注册副作用
MUST 在停用时全部撤销（无残留工具、无残留订阅、无残留触发）。

#### Scenario: 停用即撤销
- **GIVEN** 一个提供工具 `x` 与 webhook 触发的插件已启用
- **WHEN** 停用它
- **THEN** 模型侧不再有 `x`；触发器不再触发；插件区显示 disabled；无错误残留

#### Scenario: 依赖缺失不装载
- **GIVEN** 插件 B 声明 `requires: [A]`
- **WHEN** A 未启用即启用 B
- **THEN** B 保持 disabled 并给出缺失依赖原因（不半装载）

### Requirement: 三种 kind

插件 MUST 是三种 kind 之一，且每种 kind 的 `entry` 形状 MUST 与 kind 匹配：

- `in-process`：随进程编译的官方插件；清单只控制启用状态与配置。
- `sidecar`：外部进程，stdio 换行 JSON-RPC（initialize/tools/list/tools/call/health/close），
  复用 gateway 的进程监督、env 白名单与进程组回收；崩溃只影响该插件（状态 failed，可重连）。
- `webview`：UI 资源包（`ui/`），由宿主在 `/plugins/<id>/` 托管并在 WebView 中承载；
  可带 sidecar 后端。

#### Scenario: sidecar 崩溃隔离
- **WHEN** 某 sidecar 插件进程被 kill
- **THEN** 其工具调用返回明确错误、插件状态 failed；其他插件与 Harness 不受影响；可重新启用恢复

### Requirement: UI 承载与桥接

Web 目标 MUST 能打开 `/plugins/<id>/`（插件 UI）；插件 UI MUST 通过宿主桥接
（`keel.plugin.*`：读取自身状态、调用自身工具、请求审批、订阅事件）访问能力，
MUST NOT 直接获得宿主任何密钥；主题 MUST 使用宿主令牌（webview+react+solid-glass 模板）。

#### Scenario: 插件 UI 可用
- **WHEN** 打开已启用插件的 UI
- **THEN** 页面加载自 `/plugins/<id>/`，能显示自身状态并调用自身工具得到结果

### Requirement: 权限与沙箱

manifest 的 `permissions` MUST 被强制执行：工具白名单在派发处校验；
`net` 只允许声明的 host；`secrets` 只允许声明的密钥引用；sidecar 的环境变量 MUST 只给白名单。
安装/启用/升级 MUST 经 durable 审批（拒绝则不生效）。

#### Scenario: 越权被拒
- **WHEN** 插件调用未声明的工具或读取未声明的密钥
- **THEN** 调用以明确错误失败，并写审计（插件 id、动作、目标、结果）

### Requirement: 审计

每一次插件生命周期动作（install/enable/disable/uninstall/update）与越权拒绝 MUST 记录
时间、操作者、插件 id、版本、动作、结果；MUST 通过 `GET /v1/plugins/audit` 可查。

#### Scenario: 审计可查
- **WHEN** 安装→启用→停用→卸载一个插件
- **THEN** 审计按时间序返回 4 条以上记录，操作者与结果齐全

### Requirement: 创造模式（作为内置插件 `keel.creation`）

创造模式 MUST 自身是一个插件（可停用），它提供：对话式输入 → 生成插件包（webview+
react+solid-glass 模板）→ 校验 → 安装 → 启用 → 返回 UI 深链；MUST 带审批与失败回滚；
产物 MUST 可列举、可停用、可删除；生成与安装 MUST 写审计。

#### Scenario: 一句话生成插件
- **GIVEN** 已启用 `keel.creation`
- **WHEN** 用户描述「做一个展示当前会话 token 用量的小面板」
- **THEN** 生成一个 webview 插件包、安装并启用；插件区出现该插件；打开 UI 能看到数据；
  审计有 install/enable 两条记录

#### Scenario: 生成失败可回滚
- **WHEN** 生成或校验失败
- **THEN** 不产生半装状态；错误原样返回；已产生的临时产物被清理

### Requirement: TUI 行为

TUI MUST NOT 渲染插件 UI；插件行 MUST 提供「打开」，通过 `openUrl` 打开浏览器（Web 深链），
宿主不支持时 MUST 显示可复制的 URL 文本。

#### Scenario: TUI 打开插件
- **WHEN** 在 TUI 点击插件「打开」
- **THEN** 不出现 WebView/HTML 渲染；调用 openUrl（测试断言 URL）或展示 URL 文本

### Requirement: 官方插件套件覆盖矩阵

仓库 MUST 维护 `plugins/COVERAGE.md`：以 DSH 的 53 个能力组 + 25 个 bundle + 社区高星能力面
为矩阵，每行给出 keel 插件 id、波次（W1/W2/W3）与状态（implemented/enabled-by-default/
registered/absent+理由）。W1 MUST 覆盖内核 + 所有「keel 已有能力面」的官方插件化。

#### Scenario: 矩阵无空行
- **WHEN** 审查 `plugins/COVERAGE.md`
- **THEN** 每个 DSH 能力组都有映射与状态，absent 行必须有理由
