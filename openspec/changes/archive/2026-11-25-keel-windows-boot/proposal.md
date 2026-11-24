# Proposal: keel-windows-boot

> 立项：2026-11-25（Owner 实测 v0.3.0 Windows：`keel.exe start` 报 missing initdb；
> 且要求双击 exe 就完成注册/初始化/启动，无需输入 `start`。）

## Why

v0.3.0 的 Windows 自包含包已经解出内嵌 PG，但 `pgTool` 只找无后缀 `initdb`，Windows
运行时是 `initdb.exe`，于是启动失败。另外裸 `keel.exe` 仍是 usage，双击不能一键完成
注册、初始化与启动。

## What Changes

1. `pgTool`/`findPGTool` 在目录查找时同时尝试 `.exe`（Windows 及任何带后缀的运行时）。
2. Windows `keel install`：复制二进制到 `%LOCALAPPDATA%\Keel`、写 HKCU
   `Software\Classes\keel`（URL Protocol）、创建开始菜单快捷方式；不要求管理员权限。
3. 裸 `keel`（无参数）在 Windows 先完成上述注册，再走 `start`；macOS/Linux 裸命令也直接
   `start`。双击 exe → 解压运行时 → 初始化本地库 → local-only 起服 → 打开浏览器。
4. 版本 0.3.1；发布资产重建（含 Windows 修复）。

## Impact

- 代码：`cmd/keel/{db,install,start,main}.go`、单测；发布版本 0.3.1。
- 不改变协议/服务端；Windows 实机仍由 Owner 复验（本机无 Windows 运行环境）。
