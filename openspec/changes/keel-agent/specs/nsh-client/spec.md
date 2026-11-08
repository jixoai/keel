# nsh-client

## ADDED Requirements

### Requirement: NSH 多端客户端

`web/` SHALL 是 NSH（`longbridge/nsh` **pin main / nightly**，跟随官方 main，不锁 release）工程：`nsh.json` 声明 app 元数据/能力/主题，
TSX 屏幕由 `nsh` CLI 打包。第一打包目标为 **web**（静态站点，由 BFF 托管），随后
macOS/Linux/iOS/Android 复用同一代码。MUST NOT 引入第二套 UI 框架。

#### Scenario: web 打包
- **WHEN** 运行 `nsh package --target web -o dist/web/keel`
- **THEN** 产出静态站点（index.html + assets），BFF 根路径可直接打开

### Requirement: 会话列表

客户端 SHALL 提供会话列表：新建、删除、搜索、按最后活动排序、空态/加载态/错误态；
数据源为 `GET /v1/sessions`；列表项 MUST 显示最后消息摘要/时间与 run 状态（运行中/失败/取消）。

#### Scenario: 运行中标识
- **WHEN** 某会话 run 正在运行
- **THEN** 列表项显示运行中状态，完成后随事件更新

### Requirement: 会话视图

会话视图 SHALL 渲染：用户/助手消息、thinking、工具卡（名称/参数/结果/耗时，按 turn 分组）、
图片与附件、压缩标记、run 状态、`run.finished` 摘要；长列表滚动保真（进入/离开不跳位）；
MUST 支持消息级流式与（可选）token 级 partial 开关。

#### Scenario: 工具卡分组
- **WHEN** 一个 turn 内有 3 个并行工具调用
- **THEN** 显示为一个工具组（可展开），组内顺序与事件一致

### Requirement: Composer（queue/guide/附件/停止）

Composer SHALL 提供：发送、停止（stop barrier 状态）、queue/guide 模式切换（缺省 queue）、
附件上传（`POST /v1/files`）、快捷键；运行中提交按当前模式进入队列或引导；
guide 等待时 MUST 显示「等待引导当前任务…」，降级时该项回到队列且顺序不变。

#### Scenario: 模式切换即时生效
- **WHEN** 用户把模式切到 guide 并在运行中发送
- **THEN** 输入立即显示在对话流末尾并标记等待引导；服务端降级时自动回队列

### Requirement: 审批卡

客户端 SHALL 渲染 `decision` 事件为审批卡：问题、选项（一键作答）、推荐、超时/取消状态；
提交走 `POST /v1/sessions/{id}/decisions/{decisionId}`；重复提交 MUST 被禁用/幂等处理。

#### Scenario: 一键作答后继续
- **WHEN** 用户点选一个选项
- **THEN** 卡片进入 answered，run 继续；同一卡片不可再次提交

### Requirement: 主题（solid-glass）

客户端 SHALL 使用 solid-glass 设计令牌（此前已打通的 NSH 主题）作为默认皮肤：颜色/字体/
间距/圆角/动效全部来自主题令牌；MUST NOT 硬编码第三方组件语言或内联色值；暗色跟随系统。

#### Scenario: 主题切换
- **WHEN** 系统切到暗色
- **THEN** 全界面随主题令牌切换，无残留亮色块

### Requirement: 断线与通知

客户端 MUST 显示连接状态并在断线后自动重连（`after` 游标续传、不重复/不跳号）；
web 目标 MAY 使用 Web Push（service worker）提示审批/完成；通知点击 MUST 定位到对应会话。

#### Scenario: 断线重连
- **WHEN** 网络中断 10 秒后恢复
- **THEN** 会话视图补齐缺口、无重复消息、输入框恢复可用

### Requirement: TUI（同一份代码）

客户端 SHALL 通过 NSH 的 TUI target 提供 `keel tui`：同一份 NSH 代码渲染到终端
（会话列表/对话/composer/审批），键盘与鼠标可用；MUST NOT 为 TUI 另写一套 UI 逻辑
（只允许 target 适配层）。

#### Scenario: TUI 打开会话
- **WHEN** 运行 `keel tui` 并选择一个会话
- **THEN** 渲染消息/工具卡/输入框，可发送、停止、作答审批

### Requirement: 聊天列表底部对齐（NSH List alignment=bottom）

会话消息列表 MUST 使用 NSH `List` 的 `alignment=bottom`（官方 main 已提供）：新内容从底部
出现，追加时不产生整列表抖动；配合「贴底才自动跟随、离底不打断阅读」的滚动策略。

#### Scenario: 流式追加不抖动
- **WHEN** 助手消息流式追加
- **THEN** 列表保持底部对齐、无整列跳动；用户上滚后不被强制拉回
