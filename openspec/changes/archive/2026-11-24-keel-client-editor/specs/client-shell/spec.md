## ADDED Requirements

### Requirement: Composer 富文本输入

web target 的 Composer SHALL 使用 `RichEditor` 承载输入，并支持 Markdown 快捷输入与
粘贴 HTML 导入；发送时 SHALL 以 Markdown 文本提交，纯文本语义保持不变。TUI 与原生/手机
target MAY 保留纯文本 `Input`（终端不承载富文本；原生 Enter 另行接入）。

发送路径（队列/引导、附件、停止、`/learn`、`$` 补全、空态预填）MUST 与纯文本路径一致；
web 的 Enter MUST 发送、Shift+Enter MUST 换行；输入高度 MUST 自适应并在 5 行后滚动。

#### Scenario: 粘贴富文本

- **WHEN** 用户把一段带粗体/列表的 HTML 粘进 Composer
- **THEN** 输入区显示对应格式，发送的消息文本为等价 Markdown，纯文本粘贴仍按纯文本处理

#### Scenario: 终端 target

- **WHEN** 同一份代码在 `--target tui` 运行
- **THEN** Composer 仍使用纯文本输入，Enter 发送与既有键位不变
