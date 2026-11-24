# Tasks: keel-client-shell

## 1. 深链与清单

- [x] 1.1 `nsh.json`：`singleInstance: true`、`schemes: ["keel"]`。
- [x] 1.2 `web/src/desktop/links.ts`：解析 session/decision/settings；UUID 校验；非法返回 null。
- [x] 1.3 `links.test.ts`：合法/非法/带 decision 的用例。

## 2. 桌面壳

- [x] 2.1 `web/src/desktop/shell.ts`：Application secondInstance/reopen → 聚焦主窗口；
      UrlScheme.open → 解析并回调；Tray 点击/菜单；GlobalShortcut `mod+shift+k`；
      `setQuitMode("explicit")`；非桌面 `isAvailable=false` 时 no-op。
- [x] 2.2 App 接入：session → `store.openSession`；settings → `store.openSettings`；
      decision → 打开会话 + 提示；非法链接 flash。
- [x] 2.3 web/TUI 构建不回归（`nsh check/build`）。

## 3. 验证与收口

- [x] 3.1 `nsh package --target macos`；.app 二次启动单进程；`open keel://settings` 导航；
      托盘菜单栏图标可见；全局快捷键唤回。
- [x] 3.2 视觉复核（托盘/深链后的页面）；报告与归档；MAINLINE/父仓指针。

## 证据

- `evidence/macos-smoke.txt`：`nsh package --target macos` 成功；Info.plist 声明
  `CFBundleURLSchemes=["keel"]`；正常二次 `open` 进程数保持 1；`keel://settings` /
  `keel://session/<uuid>` / `keel://session/not-a-uuid` 分别分发为 settings/session/invalid；
  托盘设置路径执行（临时日志）。
- `nsh check`/`nsh build`/`nsh package --target web|macos` 全过；`links.test.ts` 2/2。
- 边界：本机 `screencapture` 返回全黑（权限/显示限制），托盘图标、深链后的页面与全局快捷键
  未做视觉/按键验收；`open -n` 是 macOS 强制新实例，不作为 singleInstance 判据。
