# Proposal: keel-client-shell

> 立项：2026-11-24（P2 客户端产品化第二切片；Owner 指示继续推进主线）。

## Why

`client-shell` 规格要求桌面客户端支持单实例、托盘、全局快捷键与 `keel://` 深链，但当前
`web/nsh.json` 没有声明 scheme/singleInstance，App 也没有接 `Application`/`Tray`/`UrlScheme`。
本切片把「再次启动聚焦已有窗口 + `keel://` 打开会话/设置 + 托盘/全局快捷键唤回」做成可用面；
原生文件关联与 updater 仍留在后续切片。

## What Changes

1. **清单**：`nsh.json` 增加 `singleInstance: true` 与 `schemes: ["keel"]`。
2. **深链解析**（纯函数，可单测）：
   - `keel://session/<uuid>` → 打开会话；
   - `keel://session/<uuid>/decision/<id>` → 打开会话并提示审批位置；
   - `keel://settings` → 设置页；
   - 其它/非法链接 → 不导航，给可读提示。
3. **桌面壳**（非 web/TUI 才生效）：`Application` 的 `secondInstance`/`reopen` 聚焦主窗口，
   `UrlScheme.open` 分发深链；托盘图标点击/菜单唤回；`mod+shift+k` 全局快捷键唤回；
   关闭最后窗口不退出（`setQuitMode("explicit")`），托盘「退出」显式退出。
4. **测试与证据**：解析单测；`nsh check/build/package --target macos`；macOS .app 冒烟
   （二次启动单进程、`open keel://settings` 导航、托盘菜单栏图标）；web/TUI 构建不受影响。

## Impact

- 代码：`web/nsh.json`、`web/src/desktop/links.ts`（新）、`web/src/desktop/shell.ts`（新）、
  `web/App.tsx`；不改服务端/API。
- 边界：文件关联、Windows/Linux 托盘差异、updater 不在本切片；`keel://` 的 decision 只定位会话
  （没有审批卡的精确滚动锚点）。
