# Tasks: keel-client-perf

- [x] 1.1 fixture 支持 `stress=1000`（500 轮）；默认 fixture 不变。
- [x] 1.2 1440/390 实测：首屏到最新、滚到顶/底、贴底跟随、输入发送。
- [x] 1.3 修复暴露的滚动/虚拟化缺陷；nsh check/build/package、TUI 回归。
- [x] 1.4 报告/证据/归档。

## 证据

- `evidence/1000-messages.txt`：window 200→500 修复、顶/底/发送实测数字、虚拟化节点数。
- `nsh check`、`nsh test --target tui App.tui.test.tsx` 14/15（既有设置页失败）。
- 边界：桌面首开 6s 级长任务；窗口固定 500 轮；未测内存。
