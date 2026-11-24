# Tasks: keel-client-editor

## 1. Composer

- [x] 1.1 web target 接入 `createRichEditor` + `RichEditor`（starter/markdown），TUI/原生保留 `Input`。
- [x] 1.2 双向同步：编辑器变更 → draft（Markdown）；外部 draft 变更（预填/补全/发送失败恢复/清空）→ 编辑器。
- [x] 1.3 保留队列/引导、附件、停止、`/learn`、`$` 补全、空态预填；发送走 Markdown。
- [x] 1.4 web host 的 DOM keydown 把 Enter 转成 `keel.send`（Shift+Enter 交编辑器换行）；
      高度自适应 5 行封顶；令牌样式；空态占位覆盖层。
- [x] 1.5 Composer 注释与 `web/README.md` 更新为 RichEditor 语义。

## 2. 测试与证据

- [x] 2.1 `nsh check`；`nsh build`；`nsh package --target web`。
- [x] 2.2 新增 web-target 用例：粘贴 HTML/加粗后 draft 为 Markdown、清空、预填；TUI 全量回归
      （已知设置页 raw JSON 1 例失败需与基线一致）。
- [x] 2.3 活实例截图：空白/输入粗体+列表/发送后消息渲染；1440 与 390；vision 复核。
- [x] 2.4 `nsh package --target web` 产物 + 公网 demo/report；报告与归档。

## 证据

- `nsh check`、`nsh build`、`nsh package --target web` 全过；`src/data/wire.test.ts` 4/4、
  `src/screens/composer-completion.test.tsx` 4/4；`App.tui.test.tsx` 14/15（唯一失败是既有的
  设置页 raw JSON 用例，非本轮回归）。
- 视觉（vision 子代理）：粗体/标题/列表在 Composer 与会话气泡均逐像素 PASS；列表 marker
  3×3、悬挂缩进 11px；用户气泡 Markdown（标题/列表/粗体）3/3 PASS；390 无横向溢出。
- 截图：`/tmp/final2-composer-bold-1440.png`、`/tmp/list-fix-1440.png`、
  `/tmp/v3-composer-heading-1440.png`、`/tmp/bubble-{heading,list,bold}.png`、
  `/tmp/three-bubbles-390.png`、`/tmp/final2-conversation-1440.png`。
- 边界：本轮 RichEditor 仅 web target；TUI/原生/手机保留纯文本 Input（Enter 已有正确语义）。
  会话列表预览已剥 Markdown（单测），但 fixture 发送后预览/时间未即时刷新，属既有数据刷新
  问题，不在本轮范围。
