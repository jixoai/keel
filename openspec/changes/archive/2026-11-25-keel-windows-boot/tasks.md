# Tasks: keel-windows-boot

- [x] 1.1 `pgTool`/`findPGTool` 支持 `.exe` 后缀；单测。
- [x] 1.2 Windows `install`：复制二进制、HKCU scheme、开始菜单快捷方式、卸载。
- [x] 1.3 无参 `keel`：Windows 先安装再 start；macOS/Linux 直接 start。
- [x] 1.4 `go test ./cmd/keel`、macOS 无参 start health 200、0.3.1 app 安装；发布 0.3.1。

## 证据

- `evidence/windows-0.3.1.txt`：Owner 的 v0.3.0 报错、`.exe` 修复、Windows install 逻辑、
  macOS 无参 start health 200 与 0.3.1 app 安装。
- `go test ./cmd/keel`（含 findPGTool `.exe` 单测）；站点 37 对/3312 链接/1465 引用。
- 边界：Windows 实机未跑，待 Owner 用 0.3.1 复验。
