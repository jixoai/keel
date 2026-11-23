# session-title Specification

## Purpose
TBD - created by archiving change keel-harness-v2. Update Purpose after archive.

## Requirements

### Requirement: 标题只来自首条用户消息

标题 MUST 从会话的第一条 `user.message` 的 `payload.text` 派生。派生规则 MUST 按顺序为：
只读 `text` 字段（MUST NOT 读取 selection/attachments 等兄弟事实）；空白串折叠为单个空格；
去除首尾空白；超过 60 个码点时截为 59 个码点加 `…`。没有用户消息或文本为空时标题 MUST 为
空字符串。派生 MUST 是纯函数：同一首条消息永远得到同一标题。

#### Scenario: 多行首条消息
- **WHEN** 首条用户消息是 `"  First line\n\nsecond   line  "`
- **THEN** 标题是 `"First line second line"`，仍是一行

#### Scenario: 超长首条消息
- **WHEN** 首条用户消息有 100 个码点
- **THEN** 标题恰好 60 个码点，以 `…` 结尾，不切坏 UTF-8

#### Scenario: 附件与选择不进入标题
- **WHEN** 首条 `user.message` 带 `selection` 与 `attachments`，`text` 为空
- **THEN** 标题为空字符串，不出现模型/附件名字

### Requirement: 标题稳定，没有隐藏状态

标题 MUST NOT 存入新的会话列或事件；它 MUST 在读取时从账本派生。后续消息 MUST NOT 改变标题。
没有重命名接口，也 MUST NOT 由 LLM 生成或缓存出一份与首条消息不一致的标题。

#### Scenario: 后续消息不移动标题
- **WHEN** 一个会话先写入首条用户消息，再写入第二条用户消息与助手回复
- **THEN** 两次读取列表得到同一标题（第一条消息的派生值）

#### Scenario: 无消息会话
- **WHEN** 一个会话没有任何 user.message
- **THEN** 列表行 `title` 是空字符串；客户端渲染「新会话 · 相对时间」，不渲染会话 id

### Requirement: 一个标题，所有读取面一致

`GET /v1/sessions` 的每一行 MUST 携带 `title` 字段（无标题时是空字符串，不是缺省键）。
消息检索命中 MUST 携带同一派生标题 `sessionTitle`；导出页 MUST 携带同一派生标题 `title`。
客户端 MUST 只在显示层做「新会话」兜底，MUST NOT 自行截断出一个服务端没有的标题。

#### Scenario: 列表与检索一致
- **WHEN** 首条消息是 `"请分析退款原因"`，列表返回该行的 `title`
- **THEN** 检索该会话的消息命中 `sessionTitle` 与列表 `title` 完全相同

#### Scenario: 客户端兜底
- **WHEN** 服务端返回空 `title`
- **THEN** 客户端显示「新会话 · 相对时间」，同一行不出现两次不同标题
