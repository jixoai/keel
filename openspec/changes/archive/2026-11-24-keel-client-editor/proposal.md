# Proposal: keel-client-editor

> 立项：2026-11-24（P2 客户端产品化第一切片；Owner 指示继续推进主线、上线更多后统一验收）。

## Why

`client-shell` 规格允许 Composer 使用 `RichEditor`（HTML 导入/导出），消息渲染使用流式 Markdown。
当前 Composer 是纯文本 `Input`：粘贴带格式的 HTML 会退化成纯文本，用户无法在输入区看到
粗体/列表/代码块等格式，也没有 Markdown 快捷输入。这是 P2 客户端产品化里最直接可见的一项。

## What Changes

1. **web target 用 RichEditor**：web 的 Composer 改用 `nsh/base` 的 `RichEditor`
   （`starter` + `markdown` 插件）；TUI 与原生/手机 target 保留现有 `Input` 纯文本路径
   （终端不承载富文本；原生 Enter 后续单独接）。
2. **输入与发送语义不变**：编辑器内容即时同步到 draft（以 Markdown 文本发送），`/learn`、
   队列/引导、附件、停止、空态预填、`$` 补全菜单全部保留。web host 在 DOM keydown 捕获
   Enter（`keel.send`），Shift+Enter 交给编辑器换行。
3. **粘贴富文本**：RichEditor 原生导入粘贴的 HTML；发送时走 Markdown，纯文本路径不变。
4. **高度与外观**：沿用 `ComposerSurface` 与令牌（字号/行高/颜色/光标/选区），`onContentSizeChange`
   自适应高度，5 行封顶；不引入新的边框或背景。

## Impact

- 代码：`web/src/screens/composer.tsx`（分支与同步）、必要的样式/类型；TUI 行为不变。
- 测试：`nsh check`、`nsh build`、`nsh test --target tui`（回归）；新增 web-target 富文本输入用例
  （粘贴/加粗/Markdown 导出）与 390/1440 视觉实拍。
- 文档：`web/README.md` 更新 Composer 描述；站点无需改架构页。
- 不涉及服务端、API 或数据库。
