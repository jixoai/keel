# Proposal: keel-self-contained

> 立项：2026-11-25（Owner 反馈 0.2.0：下载后 README 旧、起步要配数据库、官网不能一键开始。
> 目标：一个二进制下载，自动注册/解压安装成桌面应用；macOS 直接生成 .app，不走公证。）

## Why

当前发布物是「裸二进制 + 单独的 web zip」：用户还要准备 PostgreSQL、设 `KEEL_DSN`、手动起
`keel serve`，再打开浏览器；0.2.0 的 release README 还是旧的快速开始。这不是产品体验。
数据库与 web 客户端是运行必需资产，应当随二进制交付；安装注册也应由同一个二进制完成。

参考 Owner 的 reply9842 做法：单体二进制携带资产，运行时动态创建应用，后续可升级。

## What Changes

1. **单体二进制携带资产**：每个平台的 `keel` 内嵌 `pg`（PostgreSQL 18 运行树，xz 压缩）、
   `web`（NSH 打包产物）与图标/README；首次启动解压到
   `$KEEL_HOME/runtime/<version>`（macOS 也可解压进 `.app/Contents/Resources`）。
   未来二进制工具（ffmpeg、rg 等）走同一 `assets/tools` 机制。
2. **`keel start` 一键启动**：解析内嵌资产 → 自动初始化/启动本地 PostgreSQL →
   以 `auth=local-only` 起 `keel serve` → 打开浏览器；`--no-open` 供测试/无头。
   用户不需要 `KEEL_DSN`、不需要 Docker、不需要手动建库。
3. **`keel install` / `keel uninstall`**：
   - macOS：生成 `~/Applications/Keel.app`（Info.plist、`keel` scheme、ad-hoc codesign、
     `xattr -dr com.apple.quarantine`），`open` 启动；不公证。
   - Linux：生成 `~/.local/share/Keel` + `.desktop`（`x-scheme-handler/keel`）+ `~/.local/bin/keel`。
   - Windows：生成 `%LOCALAPPDATA%\Keel`、开始菜单快捷方式、HKCU `keel` scheme。
   - `uninstall` 移除注册与应用副本，默认保留用户数据；`--purge` 才删数据。
4. **构建与发布**：新增 `scripts/package-self-contained.sh`（下载 zonky/theseus 的
   relocatable PostgreSQL 18 二进制、打包 web、`go build` 单体二进制）；release 矩阵按平台
   产出 `keel-<ver>-<os>-<arch>` 单体资产并在 README/官网给「下载 → `./keel install`」。
5. **Docker 变体**：`docker compose up` 用 PG 容器；不内嵌 PG，镜像保持小。
6. **文档**：源 README 与 release README 同源更新为「一个二进制、零配置」；官网下载页
   提供单文件下载与三行起步，不再要求数据库步骤。

## Impact

- 代码：`cmd/keel/{assets,start,install}.go`、`cmd/keel/{db,main}.go`（pgTool/web 解析）、
  `go:embed` 资产占位；`scripts/package-self-contained.sh`；`.github/workflows/release.yml`。
- 资产不默认入库（体积大），CI/本地脚本生成；GitHub Release 是唯一分发出口（Alpha 零兼容）。
- 安全：只绑回环；`auth=local-only` 默认仅本机；`install` 不请求管理员权限（用户目录）。
- 平台：先交付 macOS arm64 垂直切片（当前机器可验证），Linux/Windows 随后同机制补齐；
  Docker 变体先保持 compose。
